package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/TicketsBot-cloud/database"
	"github.com/rxdn/gdl/rest"
	"github.com/rxdn/gdl/rest/request"
	"go.uber.org/zap"
)

type presence int

const (
	presenceUnknown presence = iota
	presenceAbsent
	presencePresent
)

const (
	discordCodeUnknownGuild  = 10004
	discordCodeMissingAccess = 50001
)

type presenceCheck struct {
	BotId      uint64
	Whitelabel bool
	NoToken    bool
	// Status is 0 when no HTTP response was received.
	Status int
	Body   []byte
	Err    error
}

// discordErrorCode only accepts a JSON object carrying a numeric code, so a
// Cloudflare HTML page or a proxy error can never read as a Discord answer.
func discordErrorCode(body []byte) (int, bool) {
	var parsed struct {
		Code *int `json:"code"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Code == nil {
		return 0, false
	}

	return *parsed.Code, true
}

// classifyCheck treats a guild as absent only on Discord's own Unknown Guild
// or Missing Access errors. A 401 is unknown for every bot: resetting a token
// does not remove the bot user from its guilds.
func classifyCheck(c presenceCheck) presence {
	if c.NoToken {
		return presenceUnknown
	}

	if c.Status == http.StatusOK {
		return presencePresent
	}

	code, ok := discordErrorCode(c.Body)
	if !ok {
		return presenceUnknown
	}

	switch {
	case c.Status == http.StatusNotFound && code == discordCodeUnknownGuild:
		return presenceAbsent
	case c.Status == http.StatusForbidden && code == discordCodeMissingAccess:
		return presenceAbsent
	default:
		return presenceUnknown
	}
}

// decidePresence is present if any bot is in the guild, absent only if every
// check positively says absent, and unknown otherwise.
func decidePresence(checks []presenceCheck) presence {
	if len(checks) == 0 {
		return presenceUnknown
	}

	result := presenceAbsent
	for _, c := range checks {
		switch classifyCheck(c) {
		case presencePresent:
			return presencePresent
		case presenceUnknown:
			result = presenceUnknown
		}
	}

	return result
}

func describeChecks(checks []presenceCheck) string {
	parts := make([]string, 0, len(checks))
	for _, c := range checks {
		name := "public"
		if c.Whitelabel {
			name = fmt.Sprintf("whitelabel %d", c.BotId)
		}

		switch {
		case c.NoToken:
			parts = append(parts, name+": no token")
		case c.Err != nil && c.Status == 0:
			parts = append(parts, fmt.Sprintf("%s: %v", name, c.Err))
		default:
			if code, ok := discordErrorCode(c.Body); ok {
				parts = append(parts, fmt.Sprintf("%s: %d code %d", name, c.Status, code))
			} else {
				parts = append(parts, fmt.Sprintf("%s: %d", name, c.Status))
			}
		}
	}

	return strings.Join(parts, ", ")
}

// isBotInServer checks the public bot and every whitelabel bot mapped to the
// guild. A non-nil error means presence could not be established and the guild
// must be retried on a later run.
func (d *Daemon) isBotInServer(ctx context.Context, guildId uint64) (bool, error) {
	public := d.checkGuild(ctx, d.config.MainBotToken, guildId)
	checks := []presenceCheck{public}
	if classifyCheck(public) == presencePresent {
		return true, nil
	}

	botIds, err := d.whitelabelBotsForGuild(ctx, guildId)
	if err != nil {
		return false, fmt.Errorf("fetching whitelabel bots for guild: %w", err)
	}

	for _, botId := range botIds {
		bot, err := d.whitelabelBot(ctx, botId)
		if err != nil {
			return false, fmt.Errorf("fetching whitelabel bot %d: %w", botId, err)
		}

		var check presenceCheck
		if token, err := d.whitelabelToken(bot); err != nil {
			d.logger.Warn("Could not resolve whitelabel bot token", zap.Uint64("guild", guildId), zap.Uint64("bot", botId), zap.Error(err))
			check = presenceCheck{NoToken: true}
		} else {
			check = d.checkGuild(ctx, token, guildId)
		}
		check.BotId = botId
		check.Whitelabel = true
		checks = append(checks, check)

		switch {
		case classifyCheck(check) == presencePresent:
			return true, nil
		case check.Status == http.StatusUnauthorized:
			d.logger.Warn(
				"Whitelabel bot token is invalid, so its presence in the guild cannot be checked",
				zap.Uint64("guild", guildId),
				zap.Uint64("bot", botId),
			)
		}
	}

	switch decidePresence(checks) {
	case presenceAbsent:
		return false, nil
	case presencePresent:
		return true, nil
	default:
		return false, fmt.Errorf("presence unknown (%s)", describeChecks(checks))
	}
}

func (d *Daemon) whitelabelBot(ctx context.Context, botId uint64) (database.WhitelabelBot, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	return d.database.Whitelabel.GetByBotId(ctx, botId)
}

// An error must never be read as "bot absent": it only means presence is unknown.
func (d *Daemon) whitelabelToken(bot database.WhitelabelBot) (string, error) {
	token, err := d.tokenKeyring.ResolveStored(bot.BotId, bot.Token, bot.TokenCiphertext, bot.TokenKeyVersion)
	if err != nil {
		return "", err
	}

	if token == "" {
		return "", errors.New("whitelabel bot has no token")
	}

	return token, nil
}

func (d *Daemon) whitelabelBotsForGuild(ctx context.Context, guildId uint64) ([]uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	rows, err := d.pool.Query(ctx, `SELECT "bot_id" FROM whitelabel_guilds WHERE "guild_id" = $1;`, guildId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var botIds []uint64
	for rows.Next() {
		var botId int64
		if err := rows.Scan(&botId); err != nil {
			return nil, err
		}
		botIds = append(botIds, uint64(botId))
	}

	return botIds, rows.Err()
}

func (d *Daemon) checkGuild(ctx context.Context, token string, guildId uint64) presenceCheck {
	sleepCtx(ctx, BreakTime)
	d.budget.wait(ctx)

	ctx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()

	if _, err := rest.GetGuild(ctx, token, nil, guildId); err != nil {
		var restError request.RestError
		if errors.As(err, &restError) {
			d.budget.record(time.Now(), restError.StatusCode)
			return presenceCheck{Status: restError.StatusCode, Body: restError.Raw, Err: err}
		}

		return presenceCheck{Err: err}
	}

	return presenceCheck{Status: http.StatusOK}
}

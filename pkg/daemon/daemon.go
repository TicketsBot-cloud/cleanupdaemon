package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/TicketsBot-cloud/archiverclient"
	"github.com/TicketsBot-cloud/cleanupdaemon/pkg/config"
	"github.com/TicketsBot-cloud/database"
	"github.com/rxdn/gdl/rest"
	"github.com/rxdn/gdl/rest/request"
	"go.uber.org/zap"
)

const (
	BreakTime = time.Second

	RetentionPeriod = time.Hour * 24 * 28

	// Without this the status poll below has no exit of its own.
	purgeTimeout = 5 * time.Minute

	minPollInterval = time.Second * 10
	maxPollInterval = time.Minute
)

type Daemon struct {
	logger   *zap.Logger
	config   config.Config
	client   *archiverclient.PurgingClient
	database *database.Database
}

func NewDaemon(logger *zap.Logger, config config.Config, client *archiverclient.PurgingClient, database *database.Database) *Daemon {
	return &Daemon{
		logger,
		config,
		client,
		database,
	}
}

func (d *Daemon) Run() {
	d.logger.Info("Starting run...")

	ctx := context.Background()
	guildIds, err := d.database.GuildLeaveTime.GetBefore(ctx, RetentionPeriod)
	if err != nil {
		d.logger.Error("Error while fetching guild ids", zap.Error(err))
		return
	}

	for _, guildId := range guildIds {
		logger := d.logger.With(zap.Uint64("guild", guildId))

		// Add a 1s delay as we're making a REST request
		time.Sleep(BreakTime)

		inServer, err := d.isBotInServer(ctx, guildId)
		if err != nil {
			logger.Error("error while checking if bot is in server", zap.Error(err))
			continue
		}

		if inServer {
			logger.Warn("Bot is still in server, skipping purge")

			if err := d.database.GuildLeaveTime.Delete(ctx, guildId); err != nil {
				logger.Error("Error while deleting leave time", zap.Error(err))
			}

			continue
		}

		if err := d.purgeGuild(ctx, guildId); err != nil {
			logger.Error("Failed to purge guild", zap.Error(err))
			continue
		}

		if err := d.database.GuildLeaveTime.Delete(ctx, guildId); err != nil {
			logger.Error("error while deleting leave times", zap.Error(err))
		}
	}
}

// Database first: the archiver delete is irreversible, so the other order lets a
// database failure destroy the transcripts. Both halves are idempotent.
func (d *Daemon) purgeGuild(ctx context.Context, guildId uint64) error {
	if err := d.database.PurgeGuildData(ctx, guildId, d.logger); err != nil {
		return fmt.Errorf("purging guild data from database: %w", err)
	}

	return d.purgeTranscripts(ctx, guildId)
}

func (d *Daemon) purgeTranscripts(ctx context.Context, guildId uint64) error {
	ctx, cancel := context.WithTimeout(ctx, purgeTimeout)
	defer cancel()

	if err := d.client.PurgeGuild(ctx, guildId); err != nil {
		return fmt.Errorf("sending purge request to logarchiver: %w", err)
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("timed out waiting for logarchiver purge: %w", err)
		}

		status, err := d.client.PurgeStatus(ctx, guildId)
		if err != nil {
			if errors.Is(err, archiverclient.ErrOperationNotFound) {
				return fmt.Errorf("logarchiver lost the purge operation: %w", err)
			}

			return fmt.Errorf("fetching purge status from logarchiver: %w", err)
		}

		switch status.Status {
		case archiverclient.StatusComplete:
			d.logger.Info(
				"logarchiver purge completed successfully",
				zap.Uint64("guild", guildId),
			)

			return nil

		case archiverclient.StatusFailed:
			return fmt.Errorf("logarchiver purge failed%s", formatPurgeErrors(status))

		case archiverclient.StatusTimeout:
			return errors.New("logarchiver purge timed out after inactivity")

		case archiverclient.StatusInProgress:
			d.logger.Debug(
				"Purge in progress...",
				zap.Uint64("guild", guildId),
				zap.Int("status_check_attempt", attempt),
				zap.Strings("objects", status.Removed),
				zap.Strings("failed", status.Failed),
			)

			time.Sleep(pollInterval(attempt))

		default:
			return fmt.Errorf("logarchiver returned unexpected status %q", status.Status)
		}
	}
}

func pollInterval(attempt int) time.Duration {
	interval := minPollInterval + time.Duration(attempt)*time.Second
	if interval > maxPollInterval {
		return maxPollInterval
	}

	return interval
}

func formatPurgeErrors(status archiverclient.PurgeStatus) string {
	if len(status.Errors) == 0 {
		if len(status.Failed) == 0 {
			return " (logarchiver reported no detail; check its own logs)"
		}

		return fmt.Sprintf(" (failed objects: %s)", strings.Join(status.Failed, ", "))
	}

	objects := make([]string, 0, len(status.Errors))
	for object := range status.Errors {
		objects = append(objects, object)
	}
	sort.Strings(objects)

	details := make([]string, 0, len(objects))
	for _, object := range objects {
		details = append(details, fmt.Sprintf("%s: %s", object, status.Errors[object]))
	}

	return fmt.Sprintf(" (%s)", strings.Join(details, "; "))
}

func (d *Daemon) isBotInServer(ctx context.Context, guildId uint64) (bool, error) {
	botId, ok, err := d.database.WhitelabelGuilds.GetBotByGuild(ctx, guildId)
	if err != nil {
		return false, err
	}

	var token string
	if ok {
		bot, err := d.database.Whitelabel.GetByBotId(ctx, botId)
		if err != nil {
			return false, err
		}

		token = bot.Token
	} else {
		token = d.config.MainBotToken
	}

	ctx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()

	if _, err := rest.GetGuild(ctx, token, nil, guildId); err != nil {
		var restError request.RestError
		if errors.As(err, &restError) && (restError.StatusCode == 403 || restError.StatusCode == 404) {
			return false, nil
		} else {
			return false, err
		}
	}

	return true, nil
}

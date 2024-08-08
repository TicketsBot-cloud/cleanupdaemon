package daemon

import (
	"context"
	"errors"
	"github.com/TicketsBot/archiverclient"
	"github.com/TicketsBot/cleanupdaemon/pkg/config"
	"github.com/TicketsBot/database"
	"github.com/rxdn/gdl/rest"
	"github.com/rxdn/gdl/rest/request"
	"go.uber.org/zap"
	"log"
	"math"
	"time"
)

const BreakTime = time.Second

type Daemon struct {
	logger   *zap.Logger
	config   config.Config
	client   *archiverclient.ArchiverClient
	database *database.Database
}

func NewDaemon(logger *zap.Logger, config config.Config, client *archiverclient.ArchiverClient, database *database.Database) *Daemon {
	return &Daemon{
		logger,
		config,
		client,
		database,
	}
}

func (d *Daemon) Run() {
	d.logger.Info("Starting run...")

	guildIds, err := d.database.GuildLeaveTime.GetBefore(time.Hour * 24 * 28)
	if err != nil {
		log.Printf("error occurred while fetching guild ids: %s\n", err.Error())
		return
	}

	for _, guildId := range guildIds {
		logger := d.logger.With(zap.Uint64("guild", guildId))

		// Add a 1s delay as we're making a REST request
		time.Sleep(BreakTime)

		inServer, err := d.isBotInServer(guildId)
		if err != nil {
			logger.Error("error while checking if bot is in server", zap.Error(err))
			continue
		}

		if inServer {
			logger.Warn("Bot is still in server, skipping purge")

			if err := d.database.GuildLeaveTime.Delete(guildId); err != nil {
				logger.Error("Error while deleting leave time", zap.Error(err))
			}

			continue
		}

		if d.purgeGuild(guildId) {
			if err := d.database.GuildLeaveTime.Delete(guildId); err != nil {
				logger.Error("error while deleting leave times", zap.Error(err))
			}
		}
	}
}

func (d *Daemon) purgeGuild(guildId uint64) bool {
	if err := d.client.PurgeGuild(guildId); err != nil {
		d.logger.Error("Error sending purge request", zap.Error(err), zap.Uint64("guild", guildId))
		return false
	}

	ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelFunc()

	var attempt int
	for {
		if err := ctx.Err(); err != nil {
			d.logger.Error(
				"context threw error while checking purge status",
				zap.Uint64("guild", guildId),
				zap.Error(err),
			)
			return false
		}

		status, err := d.client.PurgeStatus(guildId)
		if err != nil {
			if err == archiverclient.ErrOperationNotFound {
				d.logger.Warn(
					"logarchiver return not found when fetching purge status",
					zap.Uint64("guild", guildId),
				)
			} else {
				d.logger.Error(
					"Error when fetching purge status from logarchiver",
					zap.Uint64("guild", guildId),
					zap.Error(err),
				)
			}

			return false
		}

		if status.Status == archiverclient.StatusComplete {
			d.logger.Info(
				"logarchiver removed all transcripts successfully",
				zap.Uint64("guild", guildId),
				zap.Strings("objects", status.Removed),
			)

			return true
		} else if status.Status == archiverclient.StatusFailed {
			d.logger.Error(
				"logarchiver failed to remove all transcripts",
				zap.Uint64("guild", guildId),
				zap.Strings("success", status.Removed),
				zap.Strings("failed", status.Failed),
			)

			for objectName, errStr := range status.Errors {
				d.logger.Error(
					"logarchiver failed to remove transcript",
					zap.Uint64("guild", guildId),
					zap.String("object", objectName),
					zap.String("error", errStr),
				)
			}

			return false
		} else if status.Status == archiverclient.StatusInProgress {
			d.logger.Debug(
				"Purge in progress...",
				zap.Uint64("guild", guildId),
				zap.Int("status_check_attempt", attempt),
				zap.Strings("objects", status.Removed),
				zap.Strings("failed", status.Failed),
			)

			attempt++

			time.Sleep(time.Second * time.Duration(math.Max(10, float64(attempt))))
		}
	}
}

func (d *Daemon) isBotInServer(guildId uint64) (bool, error) {
	botId, ok, err := d.database.WhitelabelGuilds.GetBotByGuild(guildId)
	if err != nil {
		return false, err
	}

	var token string
	if ok {
		bot, err := d.database.Whitelabel.GetByBotId(botId)
		if err != nil {
			return false, err
		}

		token = bot.Token
	} else {
		token = d.config.MainBotToken
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
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

package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TicketsBot-cloud/archiverclient"
	"github.com/TicketsBot-cloud/cleanupdaemon/pkg/config"
	"github.com/TicketsBot-cloud/common/encryption"
	"github.com/TicketsBot-cloud/database"
	"github.com/jackc/pgx/v4/pgxpool"
	"go.uber.org/zap"
)

const (
	BreakTime = time.Second

	// Without this the status poll below has no exit of its own.
	purgeTimeout = 5 * time.Minute

	minPollInterval = time.Second * 10
	maxPollInterval = time.Minute

	queryTimeout   = 30 * time.Second
	dbPurgeTimeout = 2 * time.Minute

	// Rows this far past retention mean purges are failing or falling behind.
	stuckPurgeGrace = 7 * 24 * time.Hour
)

type Daemon struct {
	logger   *zap.Logger
	config   config.Config
	client   *archiverclient.PurgingClient
	database *database.Database
	pool     *pgxpool.Pool
	// nil when CACHE_URI is unset.
	cachePool *pgxpool.Pool
	discord   *discordClient
	// Shared by every purge worker and the reconciler, since Cloudflare bans by IP.
	budget       *invalidBudget
	tokenKeyring *encryption.Keyring

	// Only touched by the goroutine calling Run.
	lastUserSweep time.Time
	// Only touched by the goroutine calling Reconcile.
	lastReconcileStart time.Time
	lastPublicListed   int
}

var errLeaveCancelled = errors.New("guild_leave_time row was removed, the bot was probably re-added")

func NewDaemon(
	logger *zap.Logger,
	config config.Config,
	client *archiverclient.PurgingClient,
	database *database.Database,
	pool *pgxpool.Pool,
	cachePool *pgxpool.Pool,
	tokenKeyring *encryption.Keyring,
) *Daemon {
	budget := newInvalidBudget(logger, invalidBudgetLimit, invalidBudgetWindow)

	return &Daemon{
		logger:    logger,
		config:    config,
		client:    client,
		database:  database,
		pool:      pool,
		cachePool: cachePool,
		discord:   newDiscordClient(logger, config.DiscordProxyUrl, budget),
		budget:    budget,

		tokenKeyring: tokenKeyring,
	}
}

func (d *Daemon) Run(ctx context.Context) {
	d.logger.Info("Starting run...")

	retentionPeriod := time.Duration(d.config.RetentionPeriodDays) * 24 * time.Hour

	d.reportStuckPurges(ctx, retentionPeriod)

	if d.cachePool != nil && time.Since(d.lastUserSweep) >= userSweepInterval {
		d.sweepCacheUsers(ctx, retentionPeriod)
		d.lastUserSweep = time.Now()
	}

	guildIds, err := d.dueGuilds(ctx, retentionPeriod)
	if err != nil {
		d.logger.Error("Error while fetching guild ids", zap.Error(err))
		return
	}

	workers := max(d.config.PurgeConcurrency, 1)
	d.logger.Info("Purging due guilds", zap.Int("due", len(guildIds)), zap.Int("workers", workers))

	jobs := make(chan uint64)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for guildId := range jobs {
				d.processGuild(ctx, guildId)
			}
		}()
	}

	for _, guildId := range guildIds {
		jobs <- guildId
	}
	close(jobs)
	wg.Wait()
}

// processGuild must stay safe to call concurrently: it only shares the pools,
// the archiver client and the logger, all of which are.
func (d *Daemon) processGuild(ctx context.Context, guildId uint64) {
	logger := d.logger.With(zap.Uint64("guild", guildId))

	inServer, err := d.isBotInServer(ctx, guildId)
	if err != nil {
		logger.Error("error while checking if bot is in server", zap.Error(err))
		return
	}

	if inServer {
		logger.Warn("Bot is still in server, skipping purge")

		if err := d.deleteLeaveTime(ctx, guildId); err != nil {
			logger.Error("Error while deleting leave time", zap.Error(err))
		}

		return
	}

	if err := d.purgeGuild(ctx, guildId); err != nil {
		if errors.Is(err, errLeaveCancelled) {
			logger.Warn("Purge aborted", zap.Error(err))
			return
		}

		logger.Error("Failed to purge guild", zap.Error(err))
		return
	}

	if err := d.deleteLeaveTime(ctx, guildId); err != nil {
		logger.Error("error while deleting leave times", zap.Error(err))
	}
}

func (d *Daemon) dueGuilds(ctx context.Context, retention time.Duration) ([]uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	return d.database.GuildLeaveTime.GetBefore(ctx, retention)
}

func (d *Daemon) deleteLeaveTime(ctx context.Context, guildId uint64) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	return d.database.GuildLeaveTime.Delete(ctx, guildId)
}

// ensureStillLeft returns errLeaveCancelled when the queue row is gone, which
// the guild create listener does when the bot is re-added.
func (d *Daemon) ensureStillLeft(ctx context.Context, guildId uint64) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var exists bool
	if err := d.pool.QueryRow(
		ctx,
		`SELECT EXISTS(SELECT 1 FROM guild_leave_time WHERE "guild_id" = $1);`,
		guildId,
	).Scan(&exists); err != nil {
		return fmt.Errorf("checking guild_leave_time: %w", err)
	}

	if !exists {
		return errLeaveCancelled
	}

	return nil
}

func (d *Daemon) reportStuckPurges(ctx context.Context, retention time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var count int64
	if err := d.pool.QueryRow(
		ctx,
		`SELECT COUNT(*) FROM guild_leave_time WHERE "leave_time" < NOW() - $1::interval;`,
		retention+stuckPurgeGrace,
	).Scan(&count); err != nil {
		d.logger.Error("Error while counting overdue guild purges", zap.Error(err))
		return
	}

	if count > 0 {
		d.logger.Error(
			"Guilds are overdue for purging",
			zap.Int64("count", count),
			zap.Duration("overdue_after", retention+stuckPurgeGrace),
		)
	}
}

// The archiver delete is last because it is irreversible: any earlier failure
// leaves the leave time in place and the whole purge is retried next run. Every
// step is idempotent.
func (d *Daemon) purgeGuild(ctx context.Context, guildId uint64) error {
	if err := d.ensureStillLeft(ctx, guildId); err != nil {
		return fmt.Errorf("before database purge: %w", err)
	}

	if err := d.purgeDatabase(ctx, guildId); err != nil {
		return fmt.Errorf("purging guild data from database: %w", err)
	}

	if err := d.purgeCache(ctx, guildId); err != nil {
		return fmt.Errorf("purging guild data from cache: %w", err)
	}

	if err := d.ensureStillLeft(ctx, guildId); err != nil {
		return fmt.Errorf("before transcript purge: %w", err)
	}

	return d.purgeTranscripts(ctx, guildId)
}

func (d *Daemon) purgeDatabase(ctx context.Context, guildId uint64) error {
	ctx, cancel := context.WithTimeout(ctx, dbPurgeTimeout)
	defer cancel()

	return d.database.PurgeGuildData(ctx, guildId, d.logger)
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

			sleepCtx(ctx, pollInterval(attempt))

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

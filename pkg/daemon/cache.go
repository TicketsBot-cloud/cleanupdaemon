package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"go.uber.org/zap"
)

const (
	cachePurgeTimeout = 2 * time.Minute

	userSweepInterval  = 24 * time.Hour
	userSweepBatchSize = 5000
	userSweepPause     = 100 * time.Millisecond
	userSweepTimeout   = 30 * time.Second
)

// guilds is last only for readability: the deletes share a transaction.
var cacheGuildTables = []string{
	"channels",
	"members",
	"roles",
	"emojis",
	"voice_states",
	"guilds",
}

// withStatementTimeout runs fn in a transaction with a server-side statement
// timeout, so a slow query is cancelled by Postgres rather than left running
// after the client gives up.
func withStatementTimeout(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration, fn func(context.Context, pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d;", timeout.Milliseconds())); err != nil {
		return fmt.Errorf("setting statement timeout: %w", err)
	}

	if err := fn(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

func (d *Daemon) purgeCache(ctx context.Context, guildId uint64) error {
	if d.cachePool == nil {
		return nil
	}

	return withStatementTimeout(ctx, d.cachePool, cachePurgeTimeout, func(ctx context.Context, tx pgx.Tx) error {
		for _, table := range cacheGuildTables {
			// table comes from cacheGuildTables, never from input.
			if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE "guild_id" = $1;`, table), guildId); err != nil {
				return fmt.Errorf("deleting from cache %s: %w", table, err)
			}
		}

		return nil
	})
}

// The predicate is repeated inside the DELETE so that a user upserted between
// the batch read and the delete keeps its refreshed last_seen.
const userSweepQuery = `
WITH batch AS (
	SELECT "user_id" FROM users WHERE "user_id" > $1 ORDER BY "user_id" LIMIT $2
), deleted AS (
	DELETE FROM users u
	USING batch b
	WHERE u."user_id" = b."user_id"
		AND u."last_seen" < NOW() - $3::interval
		AND NOT EXISTS (SELECT 1 FROM members m WHERE m."user_id" = u."user_id")
	RETURNING 1
)
SELECT (SELECT MAX("user_id") FROM batch), (SELECT COUNT(*) FROM deleted);`

// sweepCacheUsers walks cache users by primary key, since last_seen has no
// index and a plain "WHERE last_seen < x LIMIT n" would rescan every surviving
// row on each batch.
func (d *Daemon) sweepCacheUsers(ctx context.Context, retention time.Duration) {
	if d.cachePool == nil {
		return
	}

	start := time.Now()
	var cursor int64
	var deleted, batches int64

	for {
		var maxId *int64
		var count int64

		err := withStatementTimeout(ctx, d.cachePool, userSweepTimeout, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, userSweepQuery, cursor, userSweepBatchSize, retention).Scan(&maxId, &count)
		})
		if err != nil {
			d.logger.Error(
				"Cache user sweep aborted",
				zap.Error(err),
				zap.Int64("cursor", cursor),
				zap.Int64("deleted", deleted),
			)
			return
		}

		batches++
		deleted += count

		if maxId == nil {
			break
		}
		cursor = *maxId

		sleepCtx(ctx, userSweepPause)
		if ctx.Err() != nil {
			return
		}
	}

	d.logger.Info(
		"Cache user sweep complete",
		zap.Int64("deleted", deleted),
		zap.Int64("batches", batches),
		zap.Duration("took", time.Since(start)),
	)
}

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"go.uber.org/zap"
)

const (
	reconcileTimeout      = 12 * time.Hour
	candidateQueryTimeout = 5 * time.Minute
	enqueueBatchSize      = 1000

	// The server counter reports the public bot's live guild count, so the
	// listing must match it closely in both directions. The cache fallback holds
	// stale rows, so it can only bound the listing from below, and loosely.
	counterTolerance = 0.02
	cacheTolerance   = 0.10

	// Guards against a listing that silently stops early between runs.
	previousListingThreshold = 0.98
)

// Guild-scoped main database tables whose rows only exist because a bot was in
// the guild. Children of tickets and panels are covered by their parents.
// user_guilds and whitelabel_allowed_guilds are left out because they hold
// guilds the bot was never in.
var candidateTables = []string{
	"tickets",
	"guild_ticket_counters",
	"ticket_labels",
	"panels",
	"multi_panels",
	"support_team",
	"forms",
	"embeds",
	"custom_integration_guilds",
	"kb_articles",
	"kb_categories",
	"kb_settings",
	"active_language",
	"audit_logs",
	"blacklist",
	"channel_category",
	"claim_settings",
	"custom_colours",
	"dashboard_onboarding",
	"guild_metadata",
	"on_call",
	"permissions",
	"premium_guilds",
	"role_blacklist",
	"role_permissions",
	"settings",
	"staff_override",
	"tags",
	"used_keys",
}

func (d *Daemon) RunReconciler(ctx context.Context, interval time.Duration) {
	for {
		if err := d.Reconcile(ctx); err != nil {
			d.logger.Error("Guild reconciliation aborted", zap.Error(err))
		}

		sleepCtx(ctx, interval)
		if ctx.Err() != nil {
			return
		}
	}
}

// Reconcile enqueues every guild we hold data for that no bot is in and that
// has no guild_leave_time row, catching leaves the worker never saw, and clears
// queued purges for guilds a bot is still in. A failed public listing aborts
// the run, since a partial list would enqueue live guilds. A failed whitelabel
// bot only removes the guilds mapped to it from both steps.
func (d *Daemon) Reconcile(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()

	if d.config.ServerCounterUrl == "" && d.cachePool == nil {
		return errors.New("neither SERVER_COUNTER_URL nor CACHE_URI is set, so the listing cannot be checked")
	}

	start := time.Now()
	d.logger.Info("Starting guild reconciliation")

	// Candidates are read before listing, so a guild that joins mid-listing has
	// no data yet and cannot be mistaken for a missed leave.
	candidates, err := d.loadCandidates(ctx)
	if err != nil {
		return fmt.Errorf("loading candidate guilds: %w", err)
	}

	publicGuilds, err := d.discord.listGuilds(ctx, d.config.MainBotToken)
	if err != nil {
		return fmt.Errorf("listing public bot guilds: %w", err)
	}

	if d.lastPublicListed > 0 {
		if err := checkSanity(len(publicGuilds), d.lastPublicListed, previousListingThreshold); err != nil {
			return fmt.Errorf("public listing shrank against the previous run: %w", err)
		}
	}

	whitelabel, err := d.listWhitelabelGuilds(ctx)
	if err != nil {
		return err
	}

	excluded, err := d.guildsMappedTo(ctx, whitelabel.failedBots)
	if err != nil {
		return fmt.Errorf("loading guilds of failed whitelabel bots: %w", err)
	}

	if err := d.checkListingSanity(ctx, publicGuilds, whitelabel.guilds); err != nil {
		return fmt.Errorf("sanity check failed: %w", err)
	}

	missing := withoutExcluded(findUnlisted(candidates, publicGuilds, whitelabel.guilds), excluded)

	interval := time.Duration(d.config.ReconcileIntervalHours) * time.Hour
	leaveTime := reconcileLeaveTime(d.lastReconcileStart, start, interval)

	enqueued, err := d.enqueueLeaves(ctx, missing, leaveTime)
	if err != nil {
		return fmt.Errorf("enqueueing missed leaves (%d enqueued before failure): %w", enqueued, err)
	}

	cleared, err := d.clearListedLeaves(ctx, start, excluded, publicGuilds, whitelabel.guilds)
	if err != nil {
		return fmt.Errorf("clearing leave times of listed guilds (%d cleared before failure): %w", cleared, err)
	}

	d.lastReconcileStart = start
	d.lastPublicListed = len(publicGuilds)

	d.logger.Info(
		"Guild reconciliation complete",
		zap.Int("listed_public", len(publicGuilds)),
		zap.Int("listed_whitelabel", len(whitelabel.guilds)),
		zap.Int("whitelabel_bots", whitelabel.bots),
		zap.Int("whitelabel_bots_failed", len(whitelabel.failedBots)),
		zap.Int("excluded_guilds", len(excluded)),
		zap.Int("candidates", len(candidates)),
		zap.Int("unlisted", len(missing)),
		zap.Int64("newly_enqueued", enqueued),
		zap.Int64("cleared_listed", cleared),
		zap.Time("leave_time", leaveTime),
		zap.Duration("took", time.Since(start)),
	)

	return nil
}

// reconcileLeaveTime backdates newly found leaves to the previous successful
// run, but never by more than one interval, so a streak of failed runs cannot
// make guilds due early.
func reconcileLeaveTime(lastStart, start time.Time, interval time.Duration) time.Time {
	if lastStart.IsZero() {
		return start
	}

	floor := start.Add(-interval)
	if lastStart.After(floor) {
		return lastStart
	}

	return floor
}

type whitelabelListing struct {
	guilds     map[uint64]struct{}
	bots       int
	failedBots []uint64
}

func (d *Daemon) listWhitelabelGuilds(ctx context.Context) (whitelabelListing, error) {
	queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	bots, err := d.database.Whitelabel.GetAll(queryCtx)
	cancel()
	if err != nil {
		return whitelabelListing{}, fmt.Errorf("fetching whitelabel bots: %w", err)
	}

	out := whitelabelListing{guilds: make(map[uint64]struct{}), bots: len(bots)}
	for _, bot := range bots {
		token, err := d.whitelabelToken(bot)
		if err != nil {
			d.logger.Error("Could not resolve whitelabel bot token, excluding its guilds from reconciliation", zap.Uint64("bot", bot.BotId), zap.Error(err))
			out.failedBots = append(out.failedBots, bot.BotId)
			continue
		}

		guilds, err := d.discord.listGuilds(ctx, token)
		if err != nil {
			if ctx.Err() != nil {
				return whitelabelListing{}, fmt.Errorf("listing whitelabel guilds: %w", ctx.Err())
			}

			level := d.logger.Error
			if errors.Is(err, errUnauthorised) {
				level = d.logger.Warn
			}
			level("Could not list whitelabel bot guilds, excluding its guilds from reconciliation", zap.Uint64("bot", bot.BotId), zap.Error(err))

			out.failedBots = append(out.failedBots, bot.BotId)
			continue
		}

		for id := range guilds {
			out.guilds[id] = struct{}{}
		}
	}

	return out, nil
}

func (d *Daemon) guildsMappedTo(ctx context.Context, botIds []uint64) (map[uint64]struct{}, error) {
	out := make(map[uint64]struct{})
	if len(botIds) == 0 {
		return out, nil
	}

	ids := make([]int64, 0, len(botIds))
	for _, id := range botIds {
		ids = append(ids, int64(id))
	}

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	rows, err := d.pool.Query(ctx, `SELECT "guild_id" FROM whitelabel_guilds WHERE "bot_id" = ANY($1::int8[]);`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[uint64(id)] = struct{}{}
	}

	return out, rows.Err()
}

func withoutExcluded(ids []uint64, excluded map[uint64]struct{}) []uint64 {
	if len(excluded) == 0 {
		return ids
	}

	var out []uint64
	for _, id := range ids {
		if _, ok := excluded[id]; !ok {
			out = append(out, id)
		}
	}

	return out
}

type sanityReference struct {
	source    string
	count     int
	listed    int
	tolerance float64
	twoSided  bool
}

func (d *Daemon) checkListingSanity(ctx context.Context, publicGuilds, whitelabelGuilds map[uint64]struct{}) error {
	ref, err := d.sanityReference(ctx, publicGuilds, whitelabelGuilds)
	if err != nil {
		return err
	}

	d.logger.Info(
		"Guild listing sanity check",
		zap.String("reference", ref.source),
		zap.Int("reference_count", ref.count),
		zap.Int("listed", ref.listed),
		zap.Float64("tolerance", ref.tolerance),
		zap.Bool("two_sided", ref.twoSided),
		zap.Int("min_reference", d.config.MinReferenceGuilds),
	)

	return checkAgainstReference(ref.listed, ref.count, d.config.MinReferenceGuilds, ref.tolerance, ref.twoSided)
}

func (d *Daemon) sanityReference(ctx context.Context, publicGuilds, whitelabelGuilds map[uint64]struct{}) (sanityReference, error) {
	if d.config.ServerCounterUrl != "" {
		source, count, err := d.fetchCounterReference(ctx)
		if err != nil {
			return sanityReference{}, fmt.Errorf("fetching server counter: %w", err)
		}

		return sanityReference{source: source, count: count, listed: len(publicGuilds), tolerance: counterTolerance, twoSided: true}, nil
	}

	if d.cachePool != nil {
		var count int64
		err := withStatementTimeout(ctx, d.cachePool, candidateQueryTimeout, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT COUNT(*) FROM guilds;`).Scan(&count)
		})
		if err != nil {
			return sanityReference{}, fmt.Errorf("counting cache guilds: %w", err)
		}

		// The cache holds whitelabel guilds too, so compare against every bot's list.
		listed := len(publicGuilds)
		for id := range whitelabelGuilds {
			if _, ok := publicGuilds[id]; !ok {
				listed++
			}
		}

		return sanityReference{source: "cache", count: int(count), listed: listed, tolerance: cacheTolerance}, nil
	}

	return sanityReference{}, errors.New("neither SERVER_COUNTER_URL nor CACHE_URI is set")
}

// fetchCounterReference prefers /total, the shards-mode count. /shards is only
// a fallback for when /total is not 200, because a resumed shard can report a
// near-zero guild count while still showing as connected.
func (d *Daemon) fetchCounterReference(ctx context.Context) (string, int, error) {
	status, body, err := d.getCounter(ctx, "/total")
	if err == nil {
		count, fallback, err := parseTotal(status, body)
		if !fallback {
			if err != nil {
				return "", 0, err
			}
			return "server_counter_total", count, nil
		}
		d.logger.Warn("Server counter /total unavailable, falling back to /shards", zap.Int("status", status))
	} else {
		d.logger.Warn("Server counter /total request failed, falling back to /shards", zap.Error(err))
	}

	status, body, err = d.getCounter(ctx, "/shards")
	if err != nil {
		return "", 0, err
	}
	if status != http.StatusOK {
		return "", 0, fmt.Errorf("unexpected status %d from server counter /shards", status)
	}

	var shards shardsResponse
	if err := json.Unmarshal(body, &shards); err != nil {
		return "", 0, fmt.Errorf("decoding shards response: %w", err)
	}

	count, err := shards.guildCount()
	if err != nil {
		return "", 0, err
	}

	return "server_counter_shards", count, nil
}

func (d *Daemon) getCounter(ctx context.Context, path string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(d.config.ServerCounterUrl, "/")+path, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := d.discord.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("requesting %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("reading %s: %w", path, err)
	}

	return resp.StatusCode, body, nil
}

// parseTotal returns fallback=true only when /total is not 200. A 200 that is
// not a usable count is an error rather than a reason to trust /shards.
func parseTotal(status int, body []byte) (int, bool, error) {
	if status != http.StatusOK {
		return 0, true, nil
	}

	var parsed struct {
		Success bool `json:"success"`
		Count   *int `json:"count"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, false, fmt.Errorf("decoding total response: %w", err)
	}

	switch {
	case !parsed.Success:
		return 0, false, errors.New("server counter /total returned success=false")
	case parsed.Count == nil:
		return 0, false, errors.New("server counter /total returned no count")
	}

	return *parsed.Count, false, nil
}

type shardsResponse struct {
	Success         bool `json:"success"`
	TotalShards     int  `json:"total_shards"`
	ConnectedShards int  `json:"connected_shards"`
	TotalGuilds     int  `json:"total_guilds"`
}

// guildCount only trusts the total when every shard is connected, since
// disconnected shards contribute nothing and would make the reference too low.
func (r shardsResponse) guildCount() (int, error) {
	switch {
	case !r.Success:
		return 0, errors.New("server counter returned success=false")
	case r.TotalShards <= 0:
		return 0, errors.New("server counter reports no shards")
	case r.ConnectedShards != r.TotalShards:
		return 0, fmt.Errorf("only %d of %d shards connected", r.ConnectedShards, r.TotalShards)
	}

	return r.TotalGuilds, nil
}

// checkAgainstReference rejects a reference below the floor, a listing more
// than tolerance below it, and with twoSided also a listing more than
// tolerance above it, which is how a near-zero reference shows up.
func checkAgainstReference(listed, reference, floor int, tolerance float64, twoSided bool) error {
	if reference < floor {
		return fmt.Errorf("reference guild count %d is below the minimum of %d", reference, floor)
	}

	if err := checkSanity(listed, reference, 1-tolerance); err != nil {
		return err
	}

	if twoSided && float64(listed) > (1+tolerance)*float64(reference) {
		return fmt.Errorf("listed %d guilds, more than %.0f%% above the reference %d", listed, tolerance*100, reference)
	}

	return nil
}

func checkSanity(listed, reference int, threshold float64) error {
	switch {
	case listed <= 0:
		return errors.New("guild listing is empty")
	case reference <= 0:
		return errors.New("reference guild count is zero")
	case float64(listed) < threshold*float64(reference):
		return fmt.Errorf("listed %d guilds, below %.0f%% of the reference %d", listed, threshold*100, reference)
	}

	return nil
}

// findUnlisted returns the candidates that appear in none of the listings, sorted.
func findUnlisted(candidates map[uint64]struct{}, listings ...map[uint64]struct{}) []uint64 {
	var out []uint64

outer:
	for id := range candidates {
		for _, listing := range listings {
			if _, ok := listing[id]; ok {
				continue outer
			}
		}
		out = append(out, id)
	}

	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// findListed returns the ids that appear in at least one listing, in input order.
func findListed(ids []uint64, listings ...map[uint64]struct{}) []uint64 {
	var out []uint64
	for _, id := range ids {
		for _, listing := range listings {
			if _, ok := listing[id]; ok {
				out = append(out, id)
				break
			}
		}
	}

	return out
}

// clearListedLeaves removes queued purges for guilds a bot is still in, which
// covers whitelabel bots with no whitelabel_guilds row that the per-guild check
// cannot see. Only rows written before the listing started are touched, so a
// leave recorded mid-listing survives.
func (d *Daemon) clearListedLeaves(ctx context.Context, listingStart time.Time, excluded map[uint64]struct{}, listings ...map[uint64]struct{}) (int64, error) {
	queued, err := d.queuedLeavesBefore(ctx, listingStart)
	if err != nil {
		return 0, err
	}

	listed := withoutExcluded(findListed(queued, listings...), excluded)

	var cleared int64
	for start := 0; start < len(listed); start += enqueueBatchSize {
		end := min(start+enqueueBatchSize, len(listed))

		batch := make([]int64, 0, end-start)
		for _, id := range listed[start:end] {
			batch = append(batch, int64(id))
		}

		queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
		tag, err := d.pool.Exec(
			queryCtx,
			`DELETE FROM guild_leave_time WHERE "guild_id" = ANY($1::int8[]) AND "leave_time" < $2;`,
			batch, listingStart,
		)
		cancel()
		if err != nil {
			return cleared, err
		}

		cleared += tag.RowsAffected()
	}

	return cleared, nil
}

func (d *Daemon) queuedLeavesBefore(ctx context.Context, before time.Time) ([]uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, candidateQueryTimeout)
	defer cancel()

	rows, err := d.pool.Query(ctx, `SELECT "guild_id" FROM guild_leave_time WHERE "leave_time" < $1;`, before)
	if err != nil {
		return nil, fmt.Errorf("querying guild_leave_time: %w", err)
	}
	defer rows.Close()

	var out []uint64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning guild_leave_time: %w", err)
		}
		out = append(out, uint64(id))
	}

	return out, rows.Err()
}

func (d *Daemon) loadCandidates(ctx context.Context) (map[uint64]struct{}, error) {
	out := make(map[uint64]struct{})

	for _, table := range candidateTables {
		ids, err := loadTableGuilds(ctx, d.pool, table)
		if err != nil {
			return nil, err
		}
		if ids == nil {
			d.logger.Debug("Candidate table does not exist, skipped", zap.String("table", table))
			continue
		}

		for _, id := range ids {
			out[id] = struct{}{}
		}
	}

	if d.cachePool != nil {
		ids, err := loadTableGuilds(ctx, d.cachePool, "guilds")
		if err != nil {
			return nil, err
		}

		for _, id := range ids {
			out[id] = struct{}{}
		}
	}

	return out, nil
}

// loadTableGuilds returns nil, nil when the table does not exist, so one
// dropped table does not abort reconciliation.
func loadTableGuilds(ctx context.Context, pool *pgxpool.Pool, table string) ([]uint64, error) {
	var out []uint64

	err := withStatementTimeout(ctx, pool, candidateQueryTimeout, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL;`, table).Scan(&exists); err != nil {
			return fmt.Errorf("checking %s exists: %w", table, err)
		}
		if !exists {
			return nil
		}

		// table comes from a constant list, never from input.
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT DISTINCT "guild_id" FROM %s WHERE "guild_id" IS NOT NULL;`, table))
		if err != nil {
			return fmt.Errorf("querying %s: %w", table, err)
		}
		defer rows.Close()

		out = []uint64{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("scanning %s: %w", table, err)
			}
			out = append(out, uint64(id))
		}

		return rows.Err()
	})

	return out, err
}

func (d *Daemon) enqueueLeaves(ctx context.Context, guildIds []uint64, leaveTime time.Time) (int64, error) {
	var enqueued int64

	for start := 0; start < len(guildIds); start += enqueueBatchSize {
		end := min(start+enqueueBatchSize, len(guildIds))

		batch := make([]int64, 0, end-start)
		for _, id := range guildIds[start:end] {
			batch = append(batch, int64(id))
		}

		queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
		tag, err := d.pool.Exec(queryCtx, `
INSERT INTO guild_leave_time("guild_id", "leave_time")
SELECT unnest($1::int8[]), $2
ON CONFLICT ("guild_id") DO NOTHING;`, batch, leaveTime)
		cancel()
		if err != nil {
			return enqueued, err
		}

		enqueued += tag.RowsAffected()
	}

	return enqueued, nil
}

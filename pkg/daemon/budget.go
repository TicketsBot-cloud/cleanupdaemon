package daemon

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Discord bans an IP through Cloudflare after 10,000 invalid requests in 10
// minutes, so we stop well short of that.
const (
	invalidBudgetLimit  = 5000
	invalidBudgetWindow = 10 * time.Minute
)

func isInvalidStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

// invalidBudget is a sliding window of invalid responses, safe for concurrent
// use. Callers that pass wait at the same moment can each record one more
// response, so the limit can be overshot by at most the number of callers.
type invalidBudget struct {
	logger *zap.Logger
	limit  int
	window time.Duration

	mu     sync.Mutex
	events []time.Time
}

func newInvalidBudget(logger *zap.Logger, limit int, window time.Duration) *invalidBudget {
	return &invalidBudget{logger: logger, limit: limit, window: window}
}

func (b *invalidBudget) prune(now time.Time) {
	cutoff := now.Add(-b.window)
	i := 0
	for i < len(b.events) && !b.events[i].After(cutoff) {
		i++
	}
	b.events = b.events[i:]
}

// delay is how long a caller must wait before it may send a request.
func (b *invalidBudget) delay(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.prune(now)
	if len(b.events) < b.limit {
		return 0
	}

	return b.events[len(b.events)-b.limit].Add(b.window).Sub(now)
}

func (b *invalidBudget) record(now time.Time, status int) {
	if !isInvalidStatus(status) {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.prune(now)
	b.events = append(b.events, now)
}

func (b *invalidBudget) wait(ctx context.Context) {
	for {
		d := b.delay(time.Now())
		if d <= 0 || ctx.Err() != nil {
			return
		}

		b.logger.Warn("Invalid request budget reached, pausing Discord requests", zap.Duration("pause", d))
		sleepCtx(ctx, d)
	}
}

package daemon

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestInvalidBudgetDelay(t *testing.T) {
	base := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		limit    int
		statuses []int
		spacing  time.Duration
		at       time.Duration
		want     time.Duration
	}{
		{"empty", 3, nil, 0, 0, 0},
		{"below limit", 3, []int{404, 403}, time.Second, 2 * time.Second, 0},
		{"valid responses ignored", 2, []int{200, 500, 404, 200}, time.Second, 4 * time.Second, 0},
		{"429 counts", 2, []int{429, 401}, time.Second, 2 * time.Second, 10*time.Minute - 2*time.Second},
		{"at limit waits for oldest to expire", 3, []int{404, 403, 401}, time.Second, 3 * time.Second, 10*time.Minute - 3*time.Second},
		{"oldest expired", 3, []int{404, 403, 401}, time.Second, 10*time.Minute + time.Second, 0},
		{"exactly at window edge", 1, []int{404}, 0, 10 * time.Minute, 0},
		{"over limit waits for enough to expire", 2, []int{404, 404, 404}, time.Second, 3 * time.Second, 10*time.Minute - 2*time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newInvalidBudget(zap.NewNop(), tt.limit, 10*time.Minute)
			for i, status := range tt.statuses {
				b.record(base.Add(time.Duration(i)*tt.spacing), status)
			}

			if got := b.delay(base.Add(tt.at)); got != tt.want {
				t.Errorf("delay() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInvalidBudgetConcurrent(t *testing.T) {
	b := newInvalidBudget(zap.NewNop(), 1000, time.Minute)
	now := time.Now()

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				b.record(now, 404)
				_ = b.delay(now)
			}
		}()
	}
	wg.Wait()

	if got := b.delay(now); got != time.Minute {
		t.Errorf("delay() after 1000 records = %v, want %v", got, time.Minute)
	}
}

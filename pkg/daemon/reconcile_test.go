package daemon

import (
	"reflect"
	"testing"
	"time"
)

func set(ids ...uint64) map[uint64]struct{} {
	out := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

func TestFindUnlisted(t *testing.T) {
	tests := []struct {
		name       string
		candidates map[uint64]struct{}
		listings   []map[uint64]struct{}
		want       []uint64
	}{
		{"no candidates", set(), []map[uint64]struct{}{set(1, 2)}, nil},
		{"no listings", set(3, 1, 2), nil, []uint64{1, 2, 3}},
		{"all listed publicly", set(1, 2), []map[uint64]struct{}{set(1, 2, 3), set()}, nil},
		{"listed by whitelabel only", set(1, 2), []map[uint64]struct{}{set(1), set(2)}, nil},
		{"some missing, sorted", set(9, 5, 1, 7), []map[uint64]struct{}{set(5), set(7)}, []uint64{1, 9}},
		{"empty listings", set(2, 1), []map[uint64]struct{}{set(), set()}, []uint64{1, 2}},
		{"large snowflakes", set(1<<63+5, 1<<62), []map[uint64]struct{}{set(1 << 62)}, []uint64{1<<63 + 5}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findUnlisted(tt.candidates, tt.listings...); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("findUnlisted() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindListed(t *testing.T) {
	tests := []struct {
		name     string
		ids      []uint64
		listings []map[uint64]struct{}
		want     []uint64
	}{
		{"no ids", nil, []map[uint64]struct{}{set(1)}, nil},
		{"no listings", []uint64{1, 2}, nil, nil},
		{"public only", []uint64{1, 2, 3}, []map[uint64]struct{}{set(2), set()}, []uint64{2}},
		{"whitelabel only", []uint64{1, 2, 3}, []map[uint64]struct{}{set(), set(3)}, []uint64{3}},
		{"in both listings counted once", []uint64{4}, []map[uint64]struct{}{set(4), set(4)}, []uint64{4}},
		{"keeps input order", []uint64{9, 1, 5}, []map[uint64]struct{}{set(1, 5, 9)}, []uint64{9, 1, 5}},
		{"none listed", []uint64{7, 8}, []map[uint64]struct{}{set(1), set(2)}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findListed(tt.ids, tt.listings...); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("findListed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckSanity(t *testing.T) {
	tests := []struct {
		name      string
		listed    int
		reference int
		threshold float64
		wantErr   bool
	}{
		{"exact match", 1000, 1000, 0.98, false},
		{"above reference", 1100, 1000, 0.98, false},
		{"at threshold", 980, 1000, 0.98, false},
		{"just below threshold", 979, 1000, 0.98, true},
		{"far below", 500, 1000, 0.98, true},
		{"cache threshold pass", 900, 1000, 0.90, false},
		{"cache threshold fail", 899, 1000, 0.90, true},
		{"empty listing", 0, 1000, 0.98, true},
		{"zero reference", 1000, 0, 0.98, true},
		{"both zero", 0, 0, 0.98, true},
		{"negative reference", 10, -1, 0.98, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSanity(tt.listed, tt.reference, tt.threshold)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkSanity() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestShardsGuildCount(t *testing.T) {
	tests := []struct {
		name    string
		resp    shardsResponse
		want    int
		wantErr bool
	}{
		{"all connected", shardsResponse{Success: true, TotalShards: 4, ConnectedShards: 4, TotalGuilds: 500}, 500, false},
		{"not successful", shardsResponse{Success: false, TotalShards: 4, ConnectedShards: 4, TotalGuilds: 500}, 0, true},
		{"no shards", shardsResponse{Success: true}, 0, true},
		{"partially connected", shardsResponse{Success: true, TotalShards: 4, ConnectedShards: 3, TotalGuilds: 375}, 0, true},
		{"connected but zero guilds", shardsResponse{Success: true, TotalShards: 1, ConnectedShards: 1}, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.resp.guildCount()
			if (err != nil) != tt.wantErr {
				t.Fatalf("guildCount() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("guildCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestReconcileLeaveTime(t *testing.T) {
	start := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	tests := []struct {
		name      string
		lastStart time.Time
		interval  time.Duration
		want      time.Time
	}{
		{"no previous run", time.Time{}, day, start},
		{"previous run one interval ago", start.Add(-day), day, start.Add(-day)},
		{"previous run within interval", start.Add(-12 * time.Hour), day, start.Add(-12 * time.Hour)},
		{"failed runs in between", start.Add(-5 * day), day, start.Add(-day)},
		{"zero interval caps at start", start.Add(-day), 0, start},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reconcileLeaveTime(tt.lastStart, start, tt.interval); !got.Equal(tt.want) {
				t.Errorf("reconcileLeaveTime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWithoutExcluded(t *testing.T) {
	tests := []struct {
		name     string
		ids      []uint64
		excluded map[uint64]struct{}
		want     []uint64
	}{
		{"nothing excluded", []uint64{1, 2}, nil, []uint64{1, 2}},
		{"empty ids", nil, set(1), nil},
		{"some excluded", []uint64{1, 2, 3}, set(2), []uint64{1, 3}},
		{"all excluded", []uint64{1, 2}, set(1, 2), nil},
		{"excluded not present", []uint64{1}, set(9), []uint64{1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withoutExcluded(tt.ids, tt.excluded); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("withoutExcluded() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckAgainstReference(t *testing.T) {
	tests := []struct {
		name      string
		listed    int
		reference int
		floor     int
		tolerance float64
		twoSided  bool
		wantErr   bool
	}{
		{"exact match", 700000, 700000, 100000, 0.02, true, false},
		{"within tolerance below", 690000, 700000, 100000, 0.02, true, false},
		{"within tolerance above", 710000, 700000, 100000, 0.02, true, false},
		{"just below lower bound", 685999, 700000, 100000, 0.02, true, true},
		{"just above upper bound", 714001, 700000, 100000, 0.02, true, true},
		{"resumed shard near-zero reference", 700000, 12, 100000, 0.02, true, true},
		{"near-zero reference with no floor still fails two-sided", 700000, 12, 0, 0.02, true, true},
		{"reference below floor", 99000, 99000, 100000, 0.02, true, true},
		{"reference at floor", 100000, 100000, 100000, 0.02, true, false},
		{"zero reference", 0, 0, 0, 0.02, true, true},
		{"empty listing", 0, 700000, 100000, 0.02, true, true},
		{"cache one-sided allows listing far above", 900000, 700000, 100000, 0.10, false, false},
		{"cache one-sided lower bound pass", 630000, 700000, 100000, 0.10, false, false},
		{"cache one-sided lower bound fail", 629999, 700000, 100000, 0.10, false, true},
		{"cache below floor", 50000, 50000, 100000, 0.10, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkAgainstReference(tt.listed, tt.reference, tt.floor, tt.tolerance, tt.twoSided)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkAgainstReference() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseTotal(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		want         int
		wantFallback bool
		wantErr      bool
	}{
		{"ok", 200, `{"success":true,"count":718936}`, 718936, false, false},
		{"ok zero is returned for the floor to reject", 200, `{"success":true,"count":0}`, 0, false, false},
		{"success false", 200, `{"success":false,"count":5}`, 0, false, true},
		{"missing count", 200, `{"success":true}`, 0, false, true},
		{"html body on 200", 200, `<html>ok</html>`, 0, false, true},
		{"empty body on 200", 200, ``, 0, false, true},
		{"503 not ready", 503, `{"success":false,"error":"server count not available yet"}`, 0, true, false},
		{"404 older counter", 404, `404 page not found`, 0, true, false},
		{"502 from proxy", 502, `<html>Bad Gateway</html>`, 0, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, fallback, err := parseTotal(tt.status, []byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseTotal() error = %v, wantErr %v", err, tt.wantErr)
			}
			if fallback != tt.wantFallback {
				t.Errorf("parseTotal() fallback = %v, want %v", fallback, tt.wantFallback)
			}
			if got != tt.want {
				t.Errorf("parseTotal() = %d, want %d", got, tt.want)
			}
		})
	}
}

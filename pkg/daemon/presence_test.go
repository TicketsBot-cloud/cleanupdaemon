package daemon

import (
	"errors"
	"testing"
)

const (
	unknownGuildBody  = `{"message": "Unknown Guild", "code": 10004}`
	missingAccessBody = `{"message": "Missing Access", "code": 50001}`
	unauthorisedBody  = `{"message": "401: Unauthorized", "code": 0}`
	cloudflareBody    = `<!DOCTYPE html><html><head><title>Access denied | discord.com used Cloudflare to restrict access</title></head><body>Error 1020</body></html>`
)

func TestClassifyCheck(t *testing.T) {
	tests := []struct {
		name  string
		check presenceCheck
		want  presence
	}{
		{"200", presenceCheck{Status: 200}, presencePresent},
		{"404 unknown guild", presenceCheck{Status: 404, Body: []byte(unknownGuildBody)}, presenceAbsent},
		{"403 missing access", presenceCheck{Status: 403, Body: []byte(missingAccessBody)}, presenceAbsent},
		{"404 with missing access code", presenceCheck{Status: 404, Body: []byte(missingAccessBody)}, presenceUnknown},
		{"403 with unknown guild code", presenceCheck{Status: 403, Body: []byte(unknownGuildBody)}, presenceUnknown},
		{"403 other code", presenceCheck{Status: 403, Body: []byte(`{"message": "Missing Permissions", "code": 50013}`)}, presenceUnknown},
		{"bare 404 empty body", presenceCheck{Status: 404}, presenceUnknown},
		{"bare 403 empty body", presenceCheck{Status: 403, Body: []byte{}}, presenceUnknown},
		{"403 cloudflare html", presenceCheck{Status: 403, Body: []byte(cloudflareBody)}, presenceUnknown},
		{"404 html from proxy", presenceCheck{Status: 404, Body: []byte("<html>404 page not found</html>")}, presenceUnknown},
		{"404 plain text from proxy", presenceCheck{Status: 404, Body: []byte("404 page not found")}, presenceUnknown},
		{"404 json without code", presenceCheck{Status: 404, Body: []byte(`{"message": "Not Found"}`)}, presenceUnknown},
		{"404 code as string", presenceCheck{Status: 404, Body: []byte(`{"code": "10004"}`)}, presenceUnknown},
		{"404 json array", presenceCheck{Status: 404, Body: []byte(`[10004]`)}, presenceUnknown},
		{"public 401", presenceCheck{Status: 401, Body: []byte(unauthorisedBody)}, presenceUnknown},
		{"whitelabel 401", presenceCheck{Whitelabel: true, Status: 401, Body: []byte(unauthorisedBody)}, presenceUnknown},
		{"429", presenceCheck{Status: 429, Body: []byte(`{"message": "You are being rate limited.", "retry_after": 1, "code": 0}`)}, presenceUnknown},
		{"500 with unknown guild body", presenceCheck{Status: 500, Body: []byte(unknownGuildBody)}, presenceUnknown},
		{"transport error", presenceCheck{Err: errors.New("dial tcp: timeout")}, presenceUnknown},
		{"no token", presenceCheck{Whitelabel: true, NoToken: true}, presenceUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCheck(tt.check); got != tt.want {
				t.Errorf("classifyCheck() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecidePresence(t *testing.T) {
	public := func(status int, body string) presenceCheck { return presenceCheck{Status: status, Body: []byte(body)} }
	wl := func(status int, body string) presenceCheck {
		return presenceCheck{BotId: 1, Whitelabel: true, Status: status, Body: []byte(body)}
	}
	gone := public(404, unknownGuildBody)
	transportErr := presenceCheck{Err: errors.New("dial tcp: timeout")}

	tests := []struct {
		name   string
		checks []presenceCheck
		want   presence
	}{
		{"no checks", nil, presenceUnknown},
		{"public 200", []presenceCheck{public(200, "")}, presencePresent},
		{"public unknown guild no whitelabel", []presenceCheck{gone}, presenceAbsent},
		{"public missing access no whitelabel", []presenceCheck{public(403, missingAccessBody)}, presenceAbsent},
		{"public cloudflare 403", []presenceCheck{public(403, cloudflareBody)}, presenceUnknown},
		{"public 401", []presenceCheck{public(401, unauthorisedBody)}, presenceUnknown},
		{"public transport error", []presenceCheck{transportErr}, presenceUnknown},
		{"whitelabel 200", []presenceCheck{gone, wl(200, "")}, presencePresent},
		{"whitelabel unknown guild", []presenceCheck{gone, wl(404, unknownGuildBody)}, presenceAbsent},
		{"whitelabel 401 is unknown", []presenceCheck{gone, wl(401, unauthorisedBody)}, presenceUnknown},
		{"whitelabel 500", []presenceCheck{gone, wl(500, "")}, presenceUnknown},
		{"whitelabel bare 404", []presenceCheck{gone, wl(404, "")}, presenceUnknown},
		{"whitelabel no token", []presenceCheck{gone, {Whitelabel: true, NoToken: true}}, presenceUnknown},
		{"public error whitelabel 200", []presenceCheck{transportErr, wl(200, "")}, presencePresent},
		{"stale row then live bot", []presenceCheck{gone, wl(404, unknownGuildBody), wl(200, "")}, presencePresent},
		{"several whitelabel all absent", []presenceCheck{gone, wl(404, unknownGuildBody), wl(403, missingAccessBody)}, presenceAbsent},
		{"several whitelabel one unknown", []presenceCheck{gone, wl(404, unknownGuildBody), wl(502, "")}, presenceUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decidePresence(tt.checks); got != tt.want {
				t.Errorf("decidePresence() = %v, want %v", got, tt.want)
			}
		})
	}
}

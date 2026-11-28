package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"
)

var errUnauthorised = errors.New("discord returned 401 unauthorised")

type discordClient struct {
	http    *http.Client
	baseUrl string
	logger  *zap.Logger
	budget  *invalidBudget
}

// Mirrors the gdl pre-request hook registered in main, so listing goes through
// the same proxy as every other Discord request.
func newDiscordClient(logger *zap.Logger, proxyUrl string, budget *invalidBudget) *discordClient {
	baseUrl := "https://discord.com/api/v10"
	if proxyUrl != "" {
		baseUrl = "http://" + proxyUrl + "/api/v10"
	}

	return &discordClient{
		http:    &http.Client{Timeout: 30 * time.Second},
		baseUrl: baseUrl,
		logger:  logger,
		budget:  budget,
	}
}

// listGuilds pages through GET /users/@me/guilds. Bots on large bot sharding
// get a 400 without the shard param, which ranges over max_concurrency from
// /gateway/bot (not the gateway shard ids).
func (c *discordClient) listGuilds(ctx context.Context, token string) (map[uint64]struct{}, error) {
	var gw struct {
		SessionStartLimit struct {
			MaxConcurrency int `json:"max_concurrency"`
		} `json:"session_start_limit"`
	}
	if err := c.get(ctx, token, c.baseUrl+"/gateway/bot", &gw); err != nil {
		return nil, fmt.Errorf("gateway/bot: %w", err)
	}

	out := make(map[uint64]struct{})
	buckets := gw.SessionStartLimit.MaxConcurrency
	if buckets <= 1 {
		if err := c.listGuildsShard(ctx, token, -1, out); err != nil {
			return nil, err
		}
		return out, nil
	}

	for shard := 0; shard < buckets; shard++ {
		if err := c.listGuildsShard(ctx, token, shard, out); err != nil {
			return nil, fmt.Errorf("shard %d: %w", shard, err)
		}
	}

	return out, nil
}

func (c *discordClient) listGuildsShard(ctx context.Context, token string, shard int, out map[uint64]struct{}) error {
	var after uint64
	for {
		url := fmt.Sprintf("%s/users/@me/guilds?limit=200&after=%d", c.baseUrl, after)
		if shard >= 0 {
			url += "&shard=" + strconv.Itoa(shard)
		}

		var page []struct {
			Id string `json:"id"`
		}
		if err := c.get(ctx, token, url, &page); err != nil {
			return err
		}

		// Discord returns short pages (198/199) mid-list, so only an empty
		// page, or a cursor that stops advancing, marks the end.
		if len(page) == 0 {
			return nil
		}

		prevAfter := after
		for _, g := range page {
			id, err := strconv.ParseUint(g.Id, 10, 64)
			if err != nil {
				return fmt.Errorf("bad guild id %q: %w", g.Id, err)
			}
			out[id] = struct{}{}
			if id > after {
				after = id
			}
		}

		if after == prevAfter {
			return fmt.Errorf("pagination cursor stuck at %d", after)
		}
	}
}

func (c *discordClient) get(ctx context.Context, token, url string, dst any) error {
	status, body, err := c.do(ctx, token, url)
	if err != nil {
		return err
	}

	switch {
	case status == http.StatusUnauthorized:
		return errUnauthorised
	case status != http.StatusOK:
		return fmt.Errorf("discord returned %d: %s", status, truncate(body, 200))
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decoding discord response: %w", err)
	}

	return nil
}

// do performs a GET, retrying on 429 and 5xx, and returns any other status.
func (c *discordClient) do(ctx context.Context, token, url string) (int, []byte, error) {
	for attempt := 0; attempt < 8; attempt++ {
		c.budget.wait(ctx)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bot "+token)
		req.Header.Set("User-Agent", "DiscordBot (https://github.com/TicketsBot-cloud/cleanupdaemon, 1.0)")

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			sleepCtx(ctx, time.Second*time.Duration(attempt+1))
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		c.budget.record(time.Now(), resp.StatusCode)
		if readErr != nil {
			return 0, nil, fmt.Errorf("reading discord response: %w", readErr)
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			_ = json.Unmarshal(body, &rl)
			wait := time.Duration(rl.RetryAfter * float64(time.Second))
			if wait <= 0 {
				wait = time.Second
			}
			c.logger.Debug("Rate limited while listing guilds", zap.Duration("retry_after", wait))
			sleepCtx(ctx, wait)
			continue
		case resp.StatusCode >= 500:
			sleepCtx(ctx, time.Second*time.Duration(attempt+1))
			continue
		}

		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			if s, err := strconv.ParseFloat(resp.Header.Get("X-RateLimit-Reset-After"), 64); err == nil {
				sleepCtx(ctx, time.Duration(s*float64(time.Second)))
			}
		}

		return resp.StatusCode, body, nil
	}

	return 0, nil, errors.New("gave up after repeated rate limits or server errors")
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

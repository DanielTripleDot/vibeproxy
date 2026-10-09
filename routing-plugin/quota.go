package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

var errQuotaUnavailable = errors.New("quota unavailable")

// These are the same read-only usage endpoints used by CLIProxyAPI's official
// management UI. No token refresh, credit redemption, or credential writes.
func fetchQuota(cred quotaCredential) (quotaSnapshot, error) {
	endpoint := "https://api.anthropic.com/api/oauth/usage"
	if cred.provider == "codex" {
		endpoint = "https://chatgpt.com/backend-api/wham/usage"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	if cred.proxyURL == "direct" || cred.proxyURL == "none" {
		transport.Proxy = nil
	} else if cred.proxyURL != "" {
		proxy, err := url.Parse(cred.proxyURL)
		if err != nil || proxy.Host == "" {
			return quotaSnapshot{}, errQuotaUnavailable
		}
		transport.Proxy = http.ProxyURL(proxy)
	}
	client := &http.Client{
		Transport: transport, Timeout: 8 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return quotaSnapshot{}, errQuotaUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+cred.token)
	req.Header.Set("Accept", "application/json")
	if cred.provider == "claude" {
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		req.Header.Set("User-Agent", "claude-cli/2.1.280 (external, cli)")
	} else {
		req.Header.Set("User-Agent", "codex-tui/0.149.1")
		if cred.accountID != "" {
			req.Header.Set("Chatgpt-Account-Id", cred.accountID)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return quotaSnapshot{}, errQuotaUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return quotaSnapshot{}, errQuotaUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return quotaSnapshot{}, errQuotaUnavailable
	}
	return parseQuota(cred.provider, raw, time.Now())
}

func parseQuota(provider string, raw []byte, observed time.Time) (quotaSnapshot, error) {
	q := quotaSnapshot{observed: observed}
	if provider == "claude" {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(raw, &payload); err != nil {
			return q, errQuotaUnavailable
		}
		for key, model := range map[string]string{
			"five_hour": "", "seven_day": "", "seven_day_oauth_apps": "",
			"seven_day_opus": "opus", "seven_day_sonnet": "sonnet", "iguana_necktie": "fable",
		} {
			data := payload[key]
			if len(data) == 0 || string(data) == "null" {
				continue
			}
			var w struct {
				Utilization *float64 `json:"utilization"`
				ResetsAt    string   `json:"resets_at"`
			}
			if err := json.Unmarshal(data, &w); err != nil || !validPercent(w.Utilization) {
				return q, errQuotaUnavailable
			}
			if w.ResetsAt == "" && *w.Utilization == 0 {
				continue
			} // unused rolling window
			reset, err := time.Parse(time.RFC3339Nano, w.ResetsAt)
			if err != nil {
				return q, errQuotaUnavailable
			}
			q.windows = append(q.windows, quotaWindow{name: key, remaining: (100 - *w.Utilization) / 100, reset: reset, model: model, long: key != "five_hour"})
		}
		// Newer Claude payloads expose scoped weekly limits in a limits array.
		var limits []struct {
			Kind     string   `json:"kind"`
			Percent  *float64 `json:"percent"`
			ResetsAt string   `json:"resets_at"`
			IsActive *bool    `json:"is_active"`
			Scope    struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		}
		if data := payload["limits"]; len(data) != 0 {
			if err := json.Unmarshal(data, &limits); err != nil {
				return q, errQuotaUnavailable
			}
			for _, limit := range limits {
				if limit.IsActive != nil && !*limit.IsActive {
					continue
				}
				if limit.Kind != "session" && limit.Kind != "weekly_all" && limit.Kind != "weekly_scoped" {
					continue
				}
				if limit.ResetsAt == "" && limit.Percent != nil && *limit.Percent == 0 {
					continue // unused rolling window
				}
				model := ""
				if limit.Kind == "weekly_scoped" {
					model = limit.Scope.Model.DisplayName
					if model == "" {
						continue
					}
				}
				reset, err := time.Parse(time.RFC3339Nano, limit.ResetsAt)
				if err != nil || !validPercent(limit.Percent) {
					return q, errQuotaUnavailable
				}
				q.windows = append(q.windows, quotaWindow{name: limit.Kind, remaining: (100 - *limit.Percent) / 100, reset: reset, model: model, long: limit.Kind != "session"})
			}
		}
	} else if provider == "codex" {
		var payload struct {
			RateLimit  *codexLimit `json:"rate_limit"`
			Additional []struct {
				LimitName string      `json:"limit_name"`
				RateLimit *codexLimit `json:"rate_limit"`
			} `json:"additional_rate_limits"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return q, errQuotaUnavailable
		}
		windows, err := codexWindows(payload.RateLimit, "", observed)
		if err != nil {
			return q, err
		}
		q.windows = append(q.windows, windows...)
		for _, extra := range payload.Additional {
			if extra.LimitName == "" {
				continue
			}
			windows, err := codexWindows(extra.RateLimit, extra.LimitName, observed)
			if err != nil {
				return q, err
			}
			q.windows = append(q.windows, windows...)
		}
	} else {
		return q, errQuotaUnavailable
	}
	if len(q.windows) == 0 {
		return q, errQuotaUnavailable
	}
	return q, nil
}

func validPercent(value *float64) bool { return value != nil && *value >= 0 && *value <= 100 }

type codexLimit struct {
	Allowed      *bool        `json:"allowed"`
	LimitReached bool         `json:"limit_reached"`
	Primary      *codexWindow `json:"primary_window"`
	Secondary    *codexWindow `json:"secondary_window"`
}
type codexWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	ResetAt       *int64   `json:"reset_at"`
	ResetAfter    *int64   `json:"reset_after_seconds"`
	WindowSeconds *int64   `json:"limit_window_seconds"`
}

func codexWindows(limit *codexLimit, model string, observed time.Time) ([]quotaWindow, error) {
	if limit == nil {
		return nil, nil
	}
	var windows []quotaWindow
	for i, w := range []*codexWindow{limit.Primary, limit.Secondary} {
		if w == nil {
			continue
		}
		if !validPercent(w.UsedPercent) {
			return nil, errQuotaUnavailable
		}
		var reset time.Time
		if w.ResetAt != nil && *w.ResetAt > 0 {
			reset = time.Unix(*w.ResetAt, 0)
		} else if w.ResetAfter != nil && *w.ResetAfter >= 0 {
			reset = observed.Add(time.Duration(*w.ResetAfter) * time.Second)
		} else {
			return nil, errQuotaUnavailable
		}
		remaining := (100 - *w.UsedPercent) / 100
		if limit.LimitReached || (limit.Allowed != nil && !*limit.Allowed) {
			remaining = 0
		}
		long := i == 1
		if w.WindowSeconds != nil {
			long = *w.WindowSeconds >= 24*60*60
		}
		name := "primary_window"
		if i == 1 {
			name = "secondary_window"
		}
		windows = append(windows, quotaWindow{name: name, remaining: remaining, reset: reset, model: model, long: long})
	}
	return windows, nil
}

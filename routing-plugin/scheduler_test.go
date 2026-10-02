package main

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func window(remaining float64, hours float64, long bool) quotaWindow {
	return quotaWindow{remaining: remaining, reset: time.Now().Add(time.Duration(hours * float64(time.Hour))), long: long}
}
func candidate(provider, id string) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{Provider: provider, ID: id, Metadata: map[string]any{"access_token": "fixture-token"}}
}
func cached(s *quotaScheduler, c pluginapi.SchedulerAuthCandidate, windows ...quotaWindow) {
	cred := credentialFor(c, "")
	fingerprint := sha256.Sum256([]byte(cred.provider + "\x00" + cred.token + "\x00" + cred.accountID + "\x00" + cred.proxyURL))
	s.entries[c.Provider+":"+c.ID] = &cacheEntry{fingerprint: fingerprint, snapshot: quotaSnapshot{observed: time.Now(), windows: windows}, nextRefresh: time.Now().Add(time.Hour)}
}
func request(provider string, candidates ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Provider: provider, Model: "claude-sonnet-4-6", Candidates: candidates}
}
func offlineScheduler() *quotaScheduler {
	return newScheduler(func(quotaCredential) (quotaSnapshot, error) { return quotaSnapshot{}, errQuotaUnavailable })
}

func TestUserExampleForBothProviders(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			s := offlineScheduler()
			a, b := candidate(provider, "6-percent-7-days"), candidate(provider, "45-percent-3-days")
			a.Priority = 100 // the opt-in quota mode replaces manual priority ranking
			cached(s, a, window(.06, 7*24, true), window(.80, 4, false))
			cached(s, b, window(.45, 3*24, true), window(.90, 3, false))
			got := s.pick(request(provider, a, b))
			if !got.Handled || got.AuthID != b.ID {
				t.Fatalf("wrong selection: %+v", got)
			}
			// Remaining quota matters, not just earliest reset.
			cached(s, a, window(.06, 3*24, true))
			cached(s, b, window(.45, 3.1*24, true))
			if got := s.pick(request(provider, a, b)); got.AuthID != b.ID {
				t.Fatalf("earliest reset defeated headroom: %+v", got)
			}
		})
	}
}

func TestShortWindowsGateButDoNotReplaceWeeklyRanking(t *testing.T) {
	s := offlineScheduler()
	a, b := candidate("claude", "a"), candidate("claude", "b")
	cached(s, a, window(.90, 24, true), window(.01, 5, false))
	cached(s, b, window(.20, 48, true), window(.90, 4, false))
	if got := s.pick(request("claude", a, b)); got.AuthID != a.ID {
		t.Fatalf("short quota distorted weekly ranking: %+v", got)
	}
	cached(s, a, window(.90, 24, true), window(0, 5, false))
	if got := s.pick(request("claude", a, b)); got.AuthID != b.ID {
		t.Fatalf("selected short-term exhausted account: %+v", got)
	}
}

func TestModelScopeAndAllExhausted(t *testing.T) {
	s := offlineScheduler()
	a := candidate("claude", "a")
	opus := window(0, 48, true)
	opus.model = "opus"
	cached(s, a, window(.45, 72, true), opus)
	if got := s.pick(request("claude", a)); got.AuthID != a.ID {
		t.Fatal("Opus quota blocked Sonnet")
	}
	req := request("claude", a)
	req.Model = "claude-opus-4-6"
	if got := s.pick(req); !got.Reject || got.RejectCode != "quota_exceeded" {
		t.Fatalf("exhaustion not enforced: %+v", got)
	}
}

func TestUnknownExpiredAndStaleDoNotInventFullQuota(t *testing.T) {
	now := time.Now()
	for _, q := range []quotaSnapshot{
		{},
		{observed: now.Add(-maxSnapshotAge - time.Second), windows: []quotaWindow{window(.9, 72, true)}},
		{observed: now, windows: []quotaWindow{{remaining: .9, reset: now.Add(-time.Second), long: true}}},
	} {
		if _, known := q.score("gpt-5.5", now); known {
			t.Fatal("invalid observation treated as known quota")
		}
	}
	s := offlineScheduler()
	defer s.shutdown()
	a := candidate("codex", "a")
	if got := s.pick(request("codex", a)); got.Handled {
		t.Fatal("cold quota must delegate")
	}
}

func TestNeverCrossProviderPoolsOrSelectIneligibleAccount(t *testing.T) {
	s := offlineScheduler()
	a, b := candidate("claude", "a"), candidate("codex", "b")
	cached(s, a, window(.4, 72, true))
	cached(s, b, window(.9, 24, true))
	if got := s.pick(request("claude", a, b)); got.Handled {
		t.Fatal("mixed pool was hijacked")
	}
	if got := s.pick(request("claude", a)); got.AuthID != a.ID {
		t.Fatal("selected absent account")
	}
	if got := s.pick(request("gemini", a)); got.Handled {
		t.Fatal("unrelated provider was hijacked")
	}
}

func TestBackgroundRefreshCoalescesAndShutdownWaits(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s := newScheduler(func(quotaCredential) (quotaSnapshot, error) {
		once.Do(func() { close(started) })
		<-release
		return quotaSnapshot{observed: time.Now(), windows: []quotaWindow{window(.45, 72, true)}}, nil
	})
	a := candidate("codex", "a")
	req := request("codex", a)
	s.pick(req)
	<-started
	for i := 0; i < 20; i++ {
		s.pick(req)
	}
	if len(s.workers) != 1 {
		t.Fatalf("refresh not coalesced: %d", len(s.workers))
	}
	done := make(chan struct{})
	go func() { s.shutdown(); close(done) }()
	select {
	case <-done:
		t.Fatal("shutdown freed live host callbacks")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown stuck")
	}
}

func TestDecodeClaudeCurrentAndLegacyWindows(t *testing.T) {
	now := time.Now()
	reset := now.Add(72 * time.Hour).Format(time.RFC3339)
	for _, raw := range []string{
		`{"five_hour":{"utilization":10,"resets_at":"` + reset + `"},"seven_day":{"utilization":55,"resets_at":"` + reset + `"},"seven_day_opus":{"utilization":100,"resets_at":"` + reset + `"}}`,
		`{"limits":[{"kind":"session","percent":10,"resets_at":"` + reset + `"},{"kind":"weekly_all","percent":55,"resets_at":"` + reset + `"},{"kind":"weekly_scoped","percent":100,"resets_at":"` + reset + `","scope":{"model":{"display_name":"Opus"}}}]}`,
	} {
		q, err := parseQuota("claude", []byte(raw), now)
		if err != nil {
			t.Fatal(err)
		}
		score, known := q.score("claude-sonnet-4-6", now)
		if !known || math.Abs(score-.45/72) > 0.00001 {
			t.Fatalf("bad score %v %v", score, known)
		}
		if score, known := q.score("claude-opus-4-6", now); !known || score != 0 {
			t.Fatal("missing model quota")
		}
	}
}

func TestDecodeCodexWindowDurationsAndSeparateModelQuotas(t *testing.T) {
	now := time.Now()
	payload := map[string]any{
		"rate_limit":             map[string]any{"allowed": true, "primary_window": map[string]any{"used_percent": 55, "limit_window_seconds": 604800, "reset_at": now.Add(72 * time.Hour).Unix()}, "secondary_window": map[string]any{"used_percent": 10, "limit_window_seconds": 18000, "reset_after_seconds": 14400}},
		"additional_rate_limits": []any{map[string]any{"limit_name": "GPT-5.5-Spark", "rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 100, "reset_after_seconds": 14400}}}},
		"code_review_rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 100}},
	}
	raw, _ := json.Marshal(payload)
	q, err := parseQuota("codex", raw, now)
	if err != nil {
		t.Fatal(err)
	}
	score, known := q.score("gpt-5.5", now)
	if !known || math.Abs(score-.45/72) > 0.00001 {
		t.Fatalf("weekly primary window not ranked: %v", score)
	}
	if score, known := q.score("gpt-5.5-spark", now); !known || score != 0 {
		t.Fatal("Spark exhaustion ignored")
	}
}

func TestMalformedQuotaIsUnknown(t *testing.T) {
	for _, raw := range []string{`{}`, `{"rate_limit":{"primary_window":{"used_percent":101,"reset_after_seconds":5}}}`, `{"rate_limit":{"primary_window":{"used_percent":10}}}`, `{"rate_limit":{"primary_window":{"reset_after_seconds":5}}}`} {
		if _, err := parseQuota("codex", []byte(raw), time.Now()); err == nil {
			t.Fatalf("accepted malformed quota: %s", raw)
		}
	}
}

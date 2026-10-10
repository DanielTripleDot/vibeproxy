package main

import (
	"testing"
	"time"
)

func TestMixedProviderAliasCannotBypassMinimum(t *testing.T) {
	s := offlineScheduler()
	defer s.shutdown()
	a, b := candidate("claude", "a"), candidate("codex", "b")
	s.configure("", map[string]float64{a.ID: 10})
	cached(s, a, window(.1, 24, true))
	cached(s, b, window(.2, 168, true))
	req := request("", a, b)
	req.Providers = []string{"claude", "codex"}
	if got := s.pick(req); got.AuthID != b.ID {
		t.Fatalf("mixed provider reserve bypassed: %+v", got)
	}
	cached(s, b)
	if got := s.pick(req); got.AuthID != b.ID {
		t.Fatalf("mixed provider unknown fallback bypassed reserve: %+v", got)
	}
}

func TestMinimumBoundaryAndPerAccountIsolation(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, remaining := range []float64{0, .099, .10, .10001, 1} {
			s := offlineScheduler()
			a, b := candidate(provider, "reserved.json"), candidate(provider, "other.json")
			s.configure("", map[string]float64{a.ID: 10})
			cached(s, a, window(remaining, 24, true))
			cached(s, b, window(.01, 168, true))
			want := a.ID
			if remaining <= .10 {
				want = b.ID
			}
			if got := s.pick(request(provider, a, b)); got.AuthID != want {
				t.Fatalf("%s at %v selected %+v, want %s", provider, remaining, got, want)
			}
			s.shutdown()
		}
	}
}

func TestMinimumGatesShortAndApplicableModelWindows(t *testing.T) {
	s := offlineScheduler()
	defer s.shutdown()
	a := candidate("claude", "a")
	s.configure("", map[string]float64{a.ID: 10})
	cached(s, a, window(.9, 168, true), window(.1, 5, false))
	if got := s.pick(request("claude", a)); !got.Reject {
		t.Fatalf("short reserve bypassed: %+v", got)
	}
	opus := window(.1, 168, true)
	opus.model = "opus"
	cached(s, a, window(.9, 168, true), opus)
	if got := s.pick(request("claude", a)); got.AuthID != a.ID {
		t.Fatalf("Opus reserve blocked Sonnet: %+v", got)
	}
	req := request("claude", a)
	req.Model = "claude-opus-4-6"
	if got := s.pick(req); !got.Reject {
		t.Fatalf("model reserve bypassed: %+v", got)
	}
}

func TestMinimumNeverFallsBackWhenQuotaUnknown(t *testing.T) {
	s := offlineScheduler()
	defer s.shutdown()
	a, b := candidate("codex", "a"), candidate("codex", "b")
	s.configure("", map[string]float64{a.ID: 10})
	// Cold start, failed refresh, stale reading, and reset without a new reading.
	for _, snapshot := range []quotaSnapshot{
		{},
		{observed: time.Now().Add(-refreshInterval - time.Second), windows: []quotaWindow{window(.9, 24, true)}},
		{observed: time.Now(), windows: []quotaWindow{window(.9, -1, true)}},
	} {
		cached(s, a)
		s.entries["codex:"+a.ID].snapshot = snapshot
		if got := s.pick(request("codex", a)); !got.Handled || !got.Reject {
			t.Fatalf("unknown reserve delegated: %+v", got)
		}
		cached(s, b)
		if got := s.pick(request("codex", a, b)); got.AuthID != b.ID {
			t.Fatalf("unknown fallback selected reserved account: %+v", got)
		}
	}
	cached(s, a, window(.11, 24, true))
	if got := s.pick(request("codex", a)); got.AuthID != a.ID {
		t.Fatalf("fresh reset reading did not restore account: %+v", got)
	}
}

func TestMinimumChangesApplyWithoutClearingObservations(t *testing.T) {
	s := offlineScheduler()
	defer s.shutdown()
	a := candidate("codex", "a")
	cached(s, a, window(.1, 24, true))
	s.configure("", map[string]float64{a.ID: 10})
	if got := s.pick(request("codex", a)); !got.Reject {
		t.Fatalf("minimum did not apply: %+v", got)
	}
	s.configure("", map[string]float64{a.ID: 0})
	if got := s.pick(request("codex", a)); got.AuthID != a.ID {
		t.Fatalf("clearing minimum did not restore account: %+v", got)
	}
	s.configure("", map[string]float64{a.ID: 100})
	cached(s, a, window(1, 24, true))
	if got := s.pick(request("codex", a)); !got.Reject {
		t.Fatalf("100%% minimum bypassed: %+v", got)
	}
}

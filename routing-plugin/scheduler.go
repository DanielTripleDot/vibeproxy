package main

import (
	"crypto/sha256"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const refreshInterval = 30 * time.Second

// Quota is only refreshed when requests arrive, so the first request after an
// idle period sees an old observation. Ranking with it while the refresh runs
// beats delegating to the backend selector, which ignores quota entirely.
const maxSnapshotAge = time.Hour

type quotaWindow struct {
	remaining float64 // fraction, not percent
	reset     time.Time
	model     string // empty means all models
	long      bool   // daily/weekly/monthly limits rank accounts; short limits gate use
}

type quotaSnapshot struct {
	windows  []quotaWindow
	observed time.Time
}

// Expired observations are unknown: a reset never implies a newly full quota.
// The most restrictive applicable window bounds sustainable usage until reset.
func (q quotaSnapshot) score(model string, now time.Time) (float64, bool) {
	if q.observed.IsZero() || now.Sub(q.observed) > maxSnapshotAge || now.Before(q.observed) {
		return 0, false
	}
	score := math.Inf(1)
	var applicable []quotaWindow
	hasLong := false
	exhausted := false
	for _, w := range q.windows {
		if w.model != "" && !strings.Contains(normalizeModel(model), normalizeModel(w.model)) {
			continue
		}
		if !w.reset.After(now) {
			return 0, false
		}
		exhausted = exhausted || w.remaining <= 0
		applicable = append(applicable, w)
		hasLong = hasLong || w.long
	}
	if exhausted {
		return 0, true
	}
	for _, w := range applicable {
		if hasLong && !w.long {
			continue
		}
		// One hour floor prevents tiny reset intervals from dominating selection.
		rate := w.remaining / math.Max(w.reset.Sub(now).Hours(), 1)
		score = math.Min(score, rate)
	}
	if math.IsInf(score, 1) {
		return 0, false
	}
	return score, true
}

func normalizeModel(model string) string {
	return strings.NewReplacer("-", "", "_", "", " ", "", ".", "").Replace(strings.ToLower(model))
}

type quotaCredential struct{ provider, token, accountID, proxyURL string }
type quotaFetcher func(quotaCredential) (quotaSnapshot, error)
type credentialLoader func(pluginapi.SchedulerAuthCandidate, string) (quotaCredential, error)
type cacheEntry struct {
	fingerprint [32]byte
	snapshot    quotaSnapshot
	nextRefresh time.Time
	lastSeen    time.Time
	lastPick    uint64
	fetching    bool
}
type quotaScheduler struct {
	mu       sync.Mutex
	entries  map[string]*cacheEntry
	fetch    quotaFetcher
	load     credentialLoader
	proxyURL string
	sequence uint64
	workers  chan struct{}
	active   sync.WaitGroup
	closed   bool
}

func newScheduler(fetch quotaFetcher) *quotaScheduler {
	return &quotaScheduler{entries: make(map[string]*cacheEntry), fetch: fetch, workers: make(chan struct{}, 8), load: func(c pluginapi.SchedulerAuthCandidate, proxy string) (quotaCredential, error) {
		cred := credentialFor(c, proxy)
		if cred.token == "" {
			return cred, errQuotaUnavailable
		}
		return cred, nil
	}}
}

func (s *quotaScheduler) configure(proxy string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if proxy != s.proxyURL {
		s.entries = make(map[string]*cacheEntry)
	}
	s.proxyURL = proxy
}
func (s *quotaScheduler) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.entries = make(map[string]*cacheEntry)
	s.mu.Unlock()
	// The host callback context remains valid only until Shutdown returns.
	s.active.Wait()
}

func credentialFor(c pluginapi.SchedulerAuthCandidate, defaultProxy string) quotaCredential {
	token, _ := c.Metadata["access_token"].(string)
	accountID, _ := c.Metadata["account_id"].(string)
	proxy, _ := c.Metadata["proxy_url"].(string)
	if proxy == "" {
		proxy = c.Attributes["proxy_url"]
	}
	if proxy == "" {
		proxy = defaultProxy
	}
	return quotaCredential{provider: c.Provider, token: token, accountID: accountID, proxyURL: proxy}
}

func (s *quotaScheduler) pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	// Do not compare percentages belonging to different providers or hijack
	// mixed custom-provider routes. OAuth accounts within one provider form a pool.
	provider := req.Provider
	if provider == "" && len(req.Providers) == 1 {
		provider = req.Providers[0]
	}
	if provider != "claude" && provider != "codex" {
		return pluginapi.SchedulerPickResponse{}
	}
	for _, c := range req.Candidates {
		if c.Provider != provider {
			return pluginapi.SchedulerPickResponse{}
		}
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return pluginapi.SchedulerPickResponse{}
	}
	var bestID string
	bestScore := -1.0
	var bestLastPick uint64
	var unknown []string
	for _, c := range req.Candidates {
		key := provider + ":" + c.ID
		cred := credentialFor(c, s.proxyURL)
		fingerprint := sha256.Sum256([]byte(cred.provider + "\x00" + cred.token + "\x00" + cred.accountID + "\x00" + cred.proxyURL))
		entry := s.entries[key]
		if entry == nil || entry.fingerprint != fingerprint {
			entry = &cacheEntry{fingerprint: fingerprint}
			s.entries[key] = entry
		}
		entry.lastSeen = now
		if !entry.fetching && !now.Before(entry.nextRefresh) {
			// Bound concurrent requests, coalesce refreshes, and never delay the
			// agent request waiting for a provider's usage endpoint.
			select {
			case s.workers <- struct{}{}:
				entry.fetching = true
				entry.nextRefresh = now.Add(refreshInterval)
				s.active.Add(1)
				go s.refresh(key, entry, c, s.proxyURL)
			default:
			}
		}
		score, known := entry.snapshot.score(req.Model, now)
		if !known {
			unknown = append(unknown, c.ID)
			continue
		}
		if score <= 0 {
			continue
		}
		if score > bestScore || (score == bestScore && (entry.lastPick < bestLastPick || (entry.lastPick == bestLastPick && c.ID < bestID))) {
			bestID, bestScore, bestLastPick = c.ID, score, entry.lastPick
		}
	}
	// Evict removed credentials; never retain credential values in cache keys.
	for key, entry := range s.entries {
		if now.Sub(entry.lastSeen) > 10*time.Minute && !entry.fetching {
			delete(s.entries, key)
		}
	}
	if bestID != "" {
		s.sequence++
		s.entries[provider+":"+bestID].lastPick = s.sequence
		return pluginapi.SchedulerPickResponse{AuthID: bestID, Handled: true}
	}
	if len(unknown) == len(req.Candidates) || len(req.Candidates) == 0 {
		// All quota observations unknown: keep configured routing and affinity.
		return pluginapi.SchedulerPickResponse{}
	}
	if len(unknown) > 0 {
		// Probe an unknown account instead of one known to be exhausted.
		s.sequence++
		bestID = unknown[int((s.sequence-1)%uint64(len(unknown)))]
		return pluginapi.SchedulerPickResponse{AuthID: bestID, Handled: true}
	}
	return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "quota_exceeded", RejectReason: "All available accounts have exhausted their observed quota; waiting for reset"}
}

func (s *quotaScheduler) refresh(key string, entry *cacheEntry, candidate pluginapi.SchedulerAuthCandidate, proxy string) {
	defer s.active.Done()
	defer func() { <-s.workers }()
	cred, err := s.load(candidate, proxy)
	var snapshot quotaSnapshot
	if err == nil {
		snapshot, err = s.fetch(cred)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[key] != entry {
		return
	} // token rotated or plugin reconfigured
	entry.fetching = false
	if err == nil {
		entry.snapshot = snapshot
	} else {
		entry.snapshot = quotaSnapshot{}
	}
	// A failed fetch clears the observation rather than inventing quota.
}

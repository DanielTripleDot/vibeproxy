package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureServer(t *testing.T, fetch quotaFetcher) *usageServer {
	t.Helper()
	dir := t.TempDir()
	write := func(name, raw string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("claude-secret@example.com.json", `{"type":"claude","access_token":"SECRET","proxy_url":"direct"}`)
	write("disabled.json", `{"type":"codex","access_token":"SECRET","disabled":true}`)
	write("other.json", `{"type":"gemini","access_token":"SECRET"}`)
	write("broken.json", `broken SECRET`)
	return &usageServer{configPath: filepath.Join(dir, "config.yaml"), defaultAuthDir: dir, fetch: fetch}
}
func callUsage(t *testing.T, s *usageServer, provider string) string {
	t.Helper()
	data, err := s.usage(provider)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(data)
	return string(raw)
}
func TestMCPUsageCacheAndRedaction(t *testing.T) {
	var calls atomic.Int32
	s := fixtureServer(t, func(c quotaCredential) (quotaSnapshot, error) {
		calls.Add(1)
		if c.token != "SECRET" || c.proxyURL != "direct" {
			t.Errorf("incorrect credential selection")
		}
		return quotaSnapshot{observed: time.Now(), windows: []quotaWindow{{remaining: .42, reset: time.Now().Add(time.Hour), long: true}}}, nil
	})
	raw := callUsage(t, s, "all")
	for _, secret := range []string{"SECRET", "secret@example.com", s.defaultAuthDir} {
		if strings.Contains(raw, secret) {
			t.Fatal("sensitive information in output")
		}
	}
	for _, want := range []string{`"remaining_percent":42`, `"state":"disabled"`, `"state":"unsupported"`, `"unreadable_or_unrecognized_files":1`, `"cached":false`} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %s in %s", want, raw)
		}
	}
	if !strings.Contains(callUsage(t, s, "claude"), `"cached":true`) || calls.Load() != 1 {
		t.Fatal("cache not reused")
	}
	// Rotation must invalidate the cached credential immediately.
	if err := os.WriteFile(filepath.Join(s.defaultAuthDir, "claude-secret@example.com.json"), []byte(`{"type":"claude","access_token":"SECRET","proxy_url":"direct","rotation":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	callUsage(t, s, "claude")
	if calls.Load() != 2 {
		t.Fatal("rotation retained old observation")
	}
	for key, c := range s.cache {
		c.snapshot.windows[0].reset = time.Now().Add(-time.Second)
		s.cache[key] = c
	}
	callUsage(t, s, "claude")
	if calls.Load() != 3 {
		t.Fatal("reset did not invalidate observation")
	}
}
func TestMCPUnavailableAndExpired(t *testing.T) {
	s := fixtureServer(t, func(quotaCredential) (quotaSnapshot, error) {
		return quotaSnapshot{}, errors.New("SECRET provider body")
	})
	raw := callUsage(t, s, "claude")
	if !strings.Contains(raw, `"state":"unavailable"`) || strings.Contains(raw, "SECRET") {
		t.Fatal(raw)
	}
	s.cache = nil
	s.fetch = func(quotaCredential) (quotaSnapshot, error) {
		return quotaSnapshot{observed: time.Now(), windows: []quotaWindow{{remaining: 1, reset: time.Now().Add(-time.Hour)}}}, nil
	}
	raw = callUsage(t, s, "claude")
	if !strings.Contains(raw, `"state":"unknown"`) {
		t.Fatal(raw)
	}
}
func TestMCPConfigAndDisabledProvider(t *testing.T) {
	s := fixtureServer(t, func(quotaCredential) (quotaSnapshot, error) {
		t.Error("disabled account fetched")
		return quotaSnapshot{}, nil
	})
	if err := os.WriteFile(s.configPath, []byte("oauth-excluded-models:\n  claude: ['*']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(callUsage(t, s, "claude"), `"state":"disabled"`) {
		t.Fatal("wildcard not honored")
	}
	if err := os.WriteFile(s.configPath, []byte("auth-dir: [invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.usage("all"); err == nil {
		t.Fatal("malformed config silently ignored")
	}
}
func TestMCPWireProtocol(t *testing.T) {
	s := fixtureServer(t, func(quotaCredential) (quotaSnapshot, error) { return quotaSnapshot{}, errors.New("SECRET") })
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_usage","arguments":{"provider":"claude"}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_usage","arguments":{"provider":"evil"}}}
{"jsonrpc":"2.0","id":5,"method":"unknown"}
BAD
{"jsonrpc":"2.0","id":6,"method":"ping"}
`
	var output bytes.Buffer
	if err := s.serve(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 7 {
		t.Fatalf("got %d replies", len(lines))
	}
	for _, line := range lines {
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) != nil || obj["jsonrpc"] != "2.0" {
			t.Fatal(line)
		}
	}
	for index, want := range map[int]string{0: `"protocolVersion":"2025-03-26"`, 1: `"readOnlyHint":true`, 2: `"structuredContent"`, 3: `"code":-32602`, 4: `"code":-32601`, 5: `"code":-32700`, 6: `"result":{}`} {
		if !strings.Contains(lines[index], want) {
			t.Errorf("missing %s in %s", want, lines[index])
		}
	}
	if strings.Contains(output.String(), "SECRET") {
		t.Fatal("leaked error")
	}
}

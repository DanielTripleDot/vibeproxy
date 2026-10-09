package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Only normalized quota data crosses the MCP boundary. Never return credentials,
// file names, provider response bodies, or raw filesystem/network errors.
type usageServer struct {
	configPath, authDir, defaultAuthDir string
	fetch                               quotaFetcher
	cache                               map[[32]byte]usageCache
}
type usageCache struct {
	snapshot quotaSnapshot
	err      error
	checked  time.Time
}
type usageWindow struct {
	Name      string  `json:"name"`
	Model     string  `json:"model_scope"`
	Kind      string  `json:"kind"`
	Remaining float64 `json:"remaining_percent"`
	Reset     string  `json:"resets_at"`
	ResetIn   int64   `json:"reset_in_seconds"`
	State     string  `json:"state"`
}
type usageAccount struct {
	ID       string        `json:"account_id"`
	Provider string        `json:"provider"`
	State    string        `json:"state"`
	Checked  string        `json:"checked_at,omitempty"`
	Cached   bool          `json:"cached"`
	Windows  []usageWindow `json:"windows"`
}

type usageConfig struct {
	AuthDir  string              `yaml:"auth-dir"`
	Proxy    string              `yaml:"proxy-url"`
	Excluded map[string][]string `yaml:"oauth-excluded-models"`
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("cannot read file")
	}
	return raw, nil
}

func (s *usageServer) configuration() (usageConfig, error) {
	cfg := usageConfig{AuthDir: s.defaultAuthDir}
	raw, err := readBounded(s.configPath)
	if err == nil {
		if yaml.Unmarshal(raw, &cfg) != nil {
			return cfg, errors.New("Cannot parse local VibeProxy configuration")
		}
	} else if !os.IsNotExist(err) {
		return cfg, errors.New("Cannot read local VibeProxy configuration")
	}
	if s.authDir != "" {
		cfg.AuthDir = s.authDir
	}
	if cfg.AuthDir == "" {
		cfg.AuthDir = s.defaultAuthDir
	}
	if strings.HasPrefix(cfg.AuthDir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return cfg, errors.New("Cannot locate home directory")
		}
		cfg.AuthDir = filepath.Join(home, cfg.AuthDir[2:])
	}
	if !filepath.IsAbs(cfg.AuthDir) {
		cfg.AuthDir = filepath.Join(filepath.Dir(s.configPath), cfg.AuthDir)
	}
	return cfg, nil
}

func (s *usageServer) usage(provider string) (any, error) {
	cfg, err := s.configuration()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(cfg.AuthDir)
	if err != nil {
		return nil, errors.New("Cannot read local VibeProxy account directory; configure --auth-dir")
	}
	if s.cache == nil {
		s.cache = make(map[[32]byte]usageCache)
	}
	accounts := make([]usageAccount, 0)
	ignored := 0
	var wg sync.WaitGroup
	var mu sync.Mutex
	workers := make(chan struct{}, 8)
	nextCache := make(map[[32]byte]usageCache)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := readBounded(filepath.Join(cfg.AuthDir, entry.Name()))
		var record struct {
			Type     string `json:"type"`
			Token    string `json:"access_token"`
			Account  string `json:"account_id"`
			Proxy    string `json:"proxy_url"`
			BaseURL  string `json:"base_url"`
			Disabled bool   `json:"disabled"`
		}
		if err != nil || json.Unmarshal(raw, &record) != nil || record.Type == "" {
			ignored++
			continue
		}
		if provider != "all" && record.Type != provider {
			continue
		}
		id := sha256.Sum256([]byte(entry.Name()))
		account := usageAccount{ID: hex.EncodeToString(id[:8]), Provider: record.Type, Windows: []usageWindow{}}
		excluded := false
		for _, model := range cfg.Excluded[record.Type] {
			if model == "*" {
				excluded = true
			}
		}
		switch {
		case record.Disabled || excluded:
			account.State = "disabled"
		case record.Type != "claude" && record.Type != "codex" || record.BaseURL != "":
			account.State = "unsupported"
		case record.Token == "":
			account.State = "unavailable"
		}
		if account.State != "" {
			mu.Lock()
			accounts = append(accounts, account)
			mu.Unlock()
			continue
		}
		proxy := record.Proxy
		if proxy == "" {
			proxy = cfg.Proxy
		}
		cred := quotaCredential{provider: record.Type, token: record.Token, accountID: record.Account, proxyURL: proxy}
		key := sha256.Sum256(append(append([]byte{}, raw...), []byte("\x00"+proxy)...))
		prior, found := s.cache[key]
		wg.Add(1)
		go func(account usageAccount, cred quotaCredential, key [32]byte, prior usageCache, found bool) {
			defer wg.Done()
			workers <- struct{}{}
			defer func() { <-workers }()
			now := time.Now()
			cached := found && now.Sub(prior.checked) >= 0 && now.Sub(prior.checked) < refreshInterval
			// A reset that has passed invalidates the cached observation immediately.
			for _, w := range prior.snapshot.windows {
				if !w.reset.After(now) {
					cached = false
				}
			}
			if !cached {
				prior.snapshot, prior.err = s.fetch(cred)
				prior.checked = time.Now()
			}
			now = time.Now()
			account.Checked = prior.checked.UTC().Format(time.RFC3339)
			account.Cached = cached
			account.State = "observed"
			if prior.err != nil {
				account.State = "unavailable"
			} else {
				for _, w := range prior.snapshot.windows {
					state := "available"
					if w.remaining <= 0 {
						state = "exhausted"
					}
					if !w.reset.After(now) {
						state = "unknown"
					}
					kind := "short_term"
					if w.long {
						kind = "long_term"
					}
					account.Windows = append(account.Windows, usageWindow{Name: w.name, Model: w.model, Kind: kind, Remaining: math.Round(w.remaining*10000) / 100, Reset: w.reset.UTC().Format(time.RFC3339), ResetIn: max(0, int64(time.Until(w.reset).Seconds())), State: state})
				}
				sort.Slice(account.Windows, func(i, j int) bool {
					a, b := account.Windows[i], account.Windows[j]
					return a.Kind+a.Model+a.Reset+a.Name < b.Kind+b.Model+b.Reset+b.Name
				})
			}
			mu.Lock()
			accounts = append(accounts, account)
			nextCache[key] = prior
			mu.Unlock()
		}(account, cred, key, prior, found)
	}
	wg.Wait()
	s.cache = nextCache
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Provider+accounts[i].ID < accounts[j].Provider+accounts[j].ID })
	host, _ := os.Hostname()
	return map[string]any{"machine": host, "as_of": time.Now().UTC().Format(time.RFC3339), "accounts": accounts, "unreadable_or_unrecognized_files": ignored, "scope": "Local VibeProxy OAuth accounts. Observed quota is not a guarantee of request success; provider toggles, model exclusions, routing and upstream errors may still prevent use."}, nil
}

func (s *usageServer) serve(input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		var result any
		var rpcError any
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			rpcError = map[string]any{"code": -32700, "message": "Parse error"}
		} else if req.JSONRPC != "2.0" || req.Method == "" {
			rpcError = map[string]any{"code": -32600, "message": "Invalid request"}
		} else {
			if len(req.ID) == 0 {
				continue
			}
			switch req.Method {
			case "initialize":
				var params struct {
					ProtocolVersion string `json:"protocolVersion"`
				}
				_ = json.Unmarshal(req.Params, &params)
				version := params.ProtocolVersion
				if version != "2024-11-05" && version != "2025-03-26" && version != "2025-06-18" && version != "2025-11-25" {
					version = "2025-03-26"
				}
				result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "vibeproxy-usage", "version": "1.0.0"}, "instructions": "Use get_usage for this machine's VibeProxy quota and reset times. Unknown or expired observations must never be interpreted as full quota."}
			case "ping":
				result = map[string]any{}
			case "tools/list":
				result = map[string]any{"tools": []any{map[string]any{"name": "get_usage", "description": "Read this machine's VibeProxy OAuth quota: remaining percentages, model scopes, reset timestamps/countdowns and check status. Claude and Codex supported. No credential refresh or changes; results cached for up to 30 seconds.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"provider": map[string]any{"type": "string", "enum": []string{"all", "claude", "codex"}, "default": "all"}}, "additionalProperties": false}, "annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "openWorldHint": true}}}}
			case "tools/call":
				var params struct {
					Name      string                     `json:"name"`
					Arguments map[string]json.RawMessage `json:"arguments"`
				}
				if json.Unmarshal(req.Params, &params) != nil || params.Name != "get_usage" {
					rpcError = map[string]any{"code": -32602, "message": "Unknown tool or invalid parameters"}
					break
				}
				provider := "all"
				valid := true
				for key, value := range params.Arguments {
					if key != "provider" || json.Unmarshal(value, &provider) != nil {
						valid = false
					}
				}
				if !valid || (provider != "all" && provider != "claude" && provider != "codex") {
					rpcError = map[string]any{"code": -32602, "message": "provider must be all, claude or codex"}
					break
				}
				data, err := s.usage(provider)
				if err != nil {
					result = map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": err.Error()}}}
				} else {
					raw, _ := json.Marshal(data)
					result = map[string]any{"content": []any{map[string]string{"type": "text", "text": string(raw)}}, "structuredContent": data, "isError": false}
				}
			default:
				rpcError = map[string]any{"code": -32601, "message": "Method not found"}
			}
		}
		var id any
		if len(req.ID) > 0 {
			_ = json.Unmarshal(req.ID, &id)
		}
		response := map[string]any{"jsonrpc": "2.0", "id": id}
		if rpcError != nil {
			response["error"] = rpcError
		} else {
			response["result"] = result
		}
		if err := encoder.Encode(response); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	return scanner.Err()
}

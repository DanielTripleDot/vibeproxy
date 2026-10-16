package management

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestVibeProxyLocalLoginIsPasswordless(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "127.0.0.2", "::1", "::ffff:127.0.0.1"} {
		t.Run(ip, func(t *testing.T) {
			h := &Handler{cfg: &config.Config{}, failedAttempts: make(map[string]*attemptInfo)}
			h.failedAttempts[ip] = &attemptInfo{}
			for _, key := range []string{"", "wrong-key", "anything"} {
				if ok, status, msg := h.AuthenticateManagementKey(ip, true, key); !ok || status != 0 || msg != "" {
					t.Fatalf("key %q: allowed=%v status=%d msg=%q", key, ok, status, msg)
				}
			}
			if len(h.failedAttempts) != 0 {
				t.Fatal("local requests retained failure state")
			}
		})
	}
}

func TestVibeProxyMiddlewareLocalBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, peer, forwarded string
		local                 bool
	}{
		{"ipv4", "127.0.0.1:1234", "", true},
		{"ipv6", "[::1]:1234", "", true},
		{"mapped", "[::ffff:127.0.0.1]:1234", "", true},
		{"remote", "203.0.113.10:1234", "", false},
		{"forwarded-remote", "127.0.0.1:1234", "203.0.113.10", false},
		{"spoofed-local", "203.0.113.10:1234", "127.0.0.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{cfg: &config.Config{}, envSecret: "correct-key", failedAttempts: make(map[string]*attemptInfo)}
			h.cfg.RemoteManagement.AllowRemote = true
			router := gin.New()
			router.GET("/test", h.Middleware(), func(c *gin.Context) { c.Status(http.StatusOK) })
			request := func(key string) int {
				req := httptest.NewRequest("GET", "/test", nil)
				req.RemoteAddr = tc.peer
				req.Host = "localhost:8318"
				req.Header.Set("Authorization", "Bearer "+key)
				if tc.forwarded != "" {
					req.Header.Set("X-Forwarded-For", tc.forwarded)
				}
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				return res.Code
			}
			for i := 0; i < 5; i++ {
				want := http.StatusUnauthorized
				if tc.local {
					want = http.StatusOK
				}
				if got := request("wrong-key"); got != want {
					t.Fatalf("bad key: %d, want %d", got, want)
				}
			}
			want := http.StatusForbidden
			if tc.local {
				want = http.StatusOK
			}
			if got := request(""); got != want {
				t.Fatalf("empty key after failures: %d, want %d", got, want)
			}
		})
	}
}

func TestVibeProxyLocalBrowserBoundary(t *testing.T) {
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"localhost:8318", "", http.StatusOK},
		{"localhost:8318", "http://localhost:8318", http.StatusOK},
		{"localhost:8318", "https://attacker.example", http.StatusForbidden},
		{"localhost:8318", "null", http.StatusForbidden},
		{"attacker.example:8318", "", http.StatusForbidden},
	} {
		h := &Handler{cfg: &config.Config{}, failedAttempts: make(map[string]*attemptInfo)}
		router := gin.New()
		router.GET("/test", h.Middleware(), func(c *gin.Context) { c.Status(http.StatusOK) })
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Host = tc.host
		req.Header.Set("Origin", tc.origin)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != tc.want {
			t.Fatalf("host=%q origin=%q: %d, want %d", tc.host, tc.origin, res.Code, tc.want)
		}
	}
}

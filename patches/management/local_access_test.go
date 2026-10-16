package management

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/crypto/bcrypt"
)

func TestVibeProxyLocalLoginNeverBanned(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-key"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"127.0.0.1", "127.0.0.2", "::1", "::ffff:127.0.0.1"} {
		for _, method := range []string{"config", "environment", "local-password"} {
			t.Run(ip+"/"+method, func(t *testing.T) {
				h := &Handler{cfg: &config.Config{}, failedAttempts: make(map[string]*attemptInfo)}
				h.cfg.RemoteManagement.SecretKey = string(hash)
				if method == "environment" {
					h.cfg.RemoteManagement.SecretKey = ""
					h.envSecret = "correct-key"
				}
				if method == "local-password" {
					h.localPassword = "correct-key"
					h.cfg.RemoteManagement.SecretKey = "unusable-hash"
				}
				// Even pre-existing state must not prevent a local retry.
				h.failedAttempts[ip] = &attemptInfo{blockedUntil: time.Now().Add(time.Hour)}
				for i := 0; i < 12; i++ {
					key := "wrong-key"
					if i%2 == 0 {
						key = ""
					}
					if ok, status, msg := h.AuthenticateManagementKey(ip, true, key); ok || status != http.StatusUnauthorized {
						t.Fatalf("attempt %d: allowed=%v status=%d msg=%q", i, ok, status, msg)
					}
				}
				if ok, status, msg := h.AuthenticateManagementKey(ip, true, "correct-key"); !ok {
					t.Fatalf("correct key rejected: %d %s", status, msg)
				}
				if len(h.failedAttempts) != 0 {
					t.Fatal("local failures retained ban state")
				}
			})
		}
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
		{"spoofed-local", "203.0.113.10:1234", "127.0.0.1", false},
		{"forwarded-remote", "127.0.0.1:1234", "203.0.113.10", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{cfg: &config.Config{}, envSecret: "correct-key", failedAttempts: make(map[string]*attemptInfo)}
			h.cfg.RemoteManagement.AllowRemote = true
			router := gin.New()
			router.GET("/test", h.Middleware(), func(c *gin.Context) { c.Status(http.StatusOK) })
			request := func(key string) int {
				req := httptest.NewRequest("GET", "/test", nil)
				req.RemoteAddr = tc.peer
				req.Header.Set("Authorization", "Bearer "+key)
				if tc.forwarded != "" {
					req.Header.Set("X-Forwarded-For", tc.forwarded)
					req.Header.Set("X-Real-IP", tc.forwarded)
				}
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				return res.Code
			}
			for i := 0; i < 5; i++ {
				if got := request("wrong-key"); got != http.StatusUnauthorized {
					t.Fatalf("bad key: %d", got)
				}
			}
			want := http.StatusForbidden
			if tc.local {
				want = http.StatusOK
			}
			if got := request("correct-key"); got != want {
				t.Fatalf("after failures: %d, want %d", got, want)
			}
			// Remote-management disabled remains effective, including spoofed headers.
			h.cfg.RemoteManagement.AllowRemote = false
			h.failedAttempts = make(map[string]*attemptInfo)
			if got := request("correct-key"); got != want {
				t.Fatalf("remote disabled: %d, want %d", got, want)
			}
		})
	}
}

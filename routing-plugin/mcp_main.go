//go:build usage_mcp

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Cannot locate home directory")
		os.Exit(1)
	}
	configPath := filepath.Join(home, ".cli-proxy-api", "merged-config.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		configPath = filepath.Join(home, ".cli-proxy-api", "config.yaml")
	}
	config := flag.String("config", configPath, "Local VibeProxy YAML config (auth-dir and proxy-url only)")
	authDir := flag.String("auth-dir", "", "Override the local account directory")
	flag.Parse()
	server := usageServer{configPath: *config, authDir: *authDir, defaultAuthDir: filepath.Join(home, ".cli-proxy-api"), fetch: fetchQuota}
	if err := server.serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "MCP stream failed")
		os.Exit(1)
	}
}

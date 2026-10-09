# Local usage MCP server

`vibeproxy-usage-mcp` gives agents a read-only `get_usage` tool for the machine on which the server runs. It reports Claude and Codex OAuth account quota, remaining percentages, provider window names, model scopes, UTC reset times, reset countdowns, and observation freshness. Other account types are marked `unsupported`.

## Build and connect

Run `make usage-mcp` (Go 1.26+). Point any stdio MCP client at the absolute path to the executable:

```json
{
  "mcpServers": {
    "vibeproxy-usage": {
      "command": "/absolute/path/to/vibe-proxy/src/Sources/Resources/Tools/vibeproxy-usage-mcp",
      "args": []
    }
  }
}
```

New app bundles also include the executable at `/Applications/VibeProxy.app/Contents/Resources/Tools/vibeproxy-usage-mcp`. Building the standalone server does not require rebuilding or restarting the running app. Client configuration is a separate step; building does not register the server in agent applications.

The process uses the home directory of the user running it. It reads `~/.cli-proxy-api/merged-config.yaml` if present, otherwise `~/.cli-proxy-api/config.yaml`, and defaults to accounts in `~/.cli-proxy-api`. `--config /path/config.yaml` selects another configuration; `--auth-dir /path/accounts` overrides its `auth-dir`. Relative account paths resolve against the configuration directory. Both global `proxy-url` and account `proxy_url` apply to provider requests. To query another machine, run the executable there through your agent host's stdio transport; it does not discover or copy remote credentials.

## Tool

Call `get_usage` with `{}` or `{"provider":"all"}`, or filter with `{"provider":"claude"}` / `{"provider":"codex"}`. Each account has a stable opaque ID derived from its local filename. Credentials, filenames, email addresses, and upstream response bodies are never returned.

- `observed` means quota was retrieved, not that inference is guaranteed to work. Each window reports `available`, `exhausted`, or `unknown`.
- `disabled` accounts and providers with a wildcard exclusion in the selected configuration are not queried. App settings appear when reflected in the merged configuration; model-specific exclusions and routing policies are not evaluated.
- `unavailable` means credentials are missing or a usage check failed. It does not mean zero or full quota. The server does not refresh expired OAuth tokens.
- `checked_at` and `cached` indicate freshness. Checks, including failed checks, are cached for up to 30 seconds within each MCP process. Credential/config changes invalidate the relevant observations; a passed reset forces another check. Passed resets never imply replenished quota.
- Window percentages are independent limits, not token balances. An empty `model_scope` applies across models; scoped windows apply only to that model family. Do not add percentages or compare them across providers. All applicable windows must allow use.

Requests go directly to the same read-only provider usage endpoints as the quota-routing plugin, using its shared decoder. Up to eight checks run concurrently, with an eight-second timeout per account. Redirects are refused. No credentials, settings, proxy processes, or account state are changed. The proxy need not be running, but current local account credentials and provider network access are required.

## Verification

```sh
cd routing-plugin
go test -race ./...
go test -race -tags usage_mcp ./...
```

These cover protocol discovery/calls, redaction, disabled/unsupported accounts, config errors, credential rotation, caching, and reset expiry, along with the existing quota parser and scheduler tests.

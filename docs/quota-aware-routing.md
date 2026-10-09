# Quota-aware account routing

Select **Settings → Account Routing → Quota-aware**. Claude OAuth accounts form one pool and Codex OAuth accounts form another. VibeProxy does not compare quota percentages across providers.

For each eligible account, the scheduler computes:

```text
score = remaining quota fraction / max(hours until reset, 1)
```

The highest score wins. Weekly and other long-term windows determine the score; every applicable short-term window must still have quota. If multiple long-term windows apply to the requested model, the lowest score controls. If an account only reports short-term windows, those windows supply its score. Equal scores alternate between accounts.

| Remaining quota | Reset in | Available quota per day |
| --- | --- | --- |
| 6% | 7 days | 0.86% |
| 45% | 3 days | 15% — preferred |

This is a headroom heuristic, not a forecast of future token consumption. Percentages are relative to each account's own allowance, so accounts on different subscription tiers are not normalized to absolute tokens.

## Availability and freshness

- CLIProxyAPI supplies candidates after its normal disabled-account, model-compatibility, and cooldown checks. Existing request retry and failover remain in the backend.
- The plugin reads current OAuth credentials through the backend's account-specific host callbacks and makes read-only usage requests. It does not refresh or write credentials. Redirects are refused, and provider error bodies and tokens are never logged.
- Claude's session, aggregate weekly, OAuth-app weekly, and model-scoped weekly limits are supported in both legacy and structured payloads. Codex window durations distinguish short and long limits; applicable model-specific quotas are separate from code-review quotas.
- Quota observations refresh on demand every 30 seconds, with at most eight concurrent requests and an eight-second request timeout. Agent requests do not wait for quota requests.
- Observations older than two minutes, failed checks, and reset times that have passed become unknown. A passed reset never implies full quota. When every account is unknown, the backend's configured selector handles the request. Known usable accounts are preferred over unknown accounts; unknown accounts are tried before known exhausted accounts.
- Global and per-account proxy settings apply to usage requests. Custom API-key providers and mixed-provider requests retain the backend's selection.

When quota data is available, this opt-in mode selects across manual account priority tiers and may change accounts between requests despite session affinity. It becomes the active scheduler ahead of other configured scheduler plugins. **Use configuration** restores the user-owned routing and plugin configuration. **Round robin** disables this quota plugin and selects the backend's round-robin strategy; other user-configured plugins remain enabled.

The selection is saved as an app preference. VibeProxy installs its bundled library into the effective plugin directory and composes runtime overrides into `merged-config.yaml`; it does not rewrite the user's `config.yaml`.

## Build and verification

`make test` builds the library and runs Swift tests and Go tests with the race detector. Source builds require Go 1.26 or later. `create-app-bundle.sh` builds the library for the same architecture as the app and signs it alongside the bundled backend.

`make test-routing` exercises account-specific credential callbacks, normal and streaming Claude/Codex requests, the example above, upstream 429 failover, and switching back to round robin. Its accounts and provider responses are synthetic and all provider traffic terminates at a local TLS fixture. On macOS it builds the exact CLIProxyAPI source version pinned in `routing-plugin/go.mod` with Go 1.27, which supports a process-local certificate override; it does not alter system trust or replace the shipped backend. The shipped backend's plugin loading is also checked separately during development.

## Public implementations investigated

The research found useful components rather than a VibeProxy fork with this complete mode:

- [VibeProxy PR #485](https://github.com/automazeio/vibeproxy/pull/485) adds Claude/Codex usage controls and current usage-payload decoding, but does not implement quota-based account selection.
- [quota-reset-router](https://github.com/WebDevCaptain/quota-reset-router/tree/62b73e7c66b40bcef7b3eb2879f99d727290a740) demonstrates CLIProxyAPI's native plugin ABI and account-specific credential access. It prefers the earliest weekly reset rather than the remaining-quota/time ratio.
- [codex-fleet-manager](https://github.com/doer-ee/cpa-plugin-codex-fleet-manager/tree/43d3c13ac5c3fe234e84bc95c539356bb846de93) and [codex-quota-scheduler](https://github.com/JefferyZhang2019/cpa-plugin-codex-quota-scheduler) use remaining-quota/time pressure for Codex.
- [TeamClaude routing](https://github.com/KarpelesLab/teamclaude/blob/master/docs/routing.md) describes expiry-pressure routing and short-window eligibility gates for Claude and Codex.

The bundled implementation uses the public scheduler API from the pinned CLIProxyAPI v8 SDK, so the existing backend binary does not need to be patched.

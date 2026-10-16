# Local management login

Direct loopback management requests do not require a password and cannot be
IP-banned. The dashboard opens automatically on localhost, 127.0.0.1, and ::1.
Its saved session uses a public marker, not a management credential.

The backend checks the socket peer, forwarded client address, and loopback Host
before granting passwordless access. Cross-origin browser requests are rejected.
Direct remote requests still require `allow-remote` and a valid management key,
and retain the five-failure ban. A raw TCP tunnel that hides the original peer
is indistinguishable from a local client and must preserve a non-loopback source.
The Mac VM bridge uses `TCP:127.0.0.1:8318,bind=10.211.55.2`; the backend therefore
sees that traffic as remote even if a VM client spoofs a localhost Host header.
Keep this source binding when changing or recreating that bridge.

`scripts/build-management-backend.py` builds the version pinned in
`routing-plugin/go.mod`, checks its checksum against `go.sum`, and applies
`patches/management/localhost-no-ban.patch`. The module cache and tracked upstream
binary are not changed. `make app` packages this patched executable under the
existing `cli-proxy-api-plus` name. Its version has a `-vibeproxy-local` suffix
and its commit field includes the patch hash.

Run `make test-management` for race-enabled management tests and `make test` for
those tests plus Swift and routing tests. `make test-routing` exercises the patched
backend with synthetic provider accounts. It requires Go 1.27 for process-local
TLS trust on macOS.

The dashboard bootstrap is inserted when serving its HTML to loopback clients,
so an upstream dashboard asset refresh retains local auto-login. It currently
uses the upstream `cli-proxy-auth` storage contract; verify browser auto-login
when updating the upstream UI. Install and restart the app to apply a new build.

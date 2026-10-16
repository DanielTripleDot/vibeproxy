# Local management login

Direct loopback management requests can always retry a login. Missing or incorrect
keys still return HTTP 401, but they never trigger the backend's 30-minute IP ban.
A correct management key works immediately, including when ban state already
exists. This applies to IPv4 and IPv6 loopback addresses. Remote management still
requires `allow-remote`, a valid key, and remains subject to the five-failure ban.

The HTTP middleware checks both the socket peer and the forwarded client address
before granting the exemption. A remote peer claiming `X-Forwarded-For: 127.0.0.1`
does not qualify. A raw TCP tunnel that hides the original peer appears local;
management keys are therefore still required for all requests.

`scripts/build-management-backend.py` downloads the SDK version pinned in
`routing-plugin/go.mod`, checks it against `go.sum`, copies it into a temporary
build directory, and applies `patches/management/localhost-no-ban.patch`. The Go
module cache and tracked upstream executable are not edited. `make app` packages
the patched executable under the existing `cli-proxy-api-plus` name and signs it
normally. Its version has a `-vibeproxy-local` suffix and its commit field includes
the patch hash. An upstream version update must also pass patch application and
the management tests; do not replace this build with an unpatched download.

Run `make test-management` for the race-enabled management package tests, including
repeated failures followed by a correct key, old ban state, configured/environment/
local passwords, IPv6, remote bans, and forwarded-header spoofing. `make test` also
runs these checks. `make test-routing` exercises the patched backend with synthetic
provider accounts; it requires Go 1.27 for process-local TLS trust on macOS.

Installing the new app and restarting its backend is required to change an already
running installation. Rebuilding the bundle alone does not clear a live lockout.

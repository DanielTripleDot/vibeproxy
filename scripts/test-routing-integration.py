#!/usr/bin/env python3
"""Exercise the shipped CLIProxyAPI C ABI using isolated, synthetic OAuth accounts.

All provider traffic terminates at a local TLS mock. No real credentials, provider
requests, running VibeProxy processes, or user configuration are used.
"""
import http.server
import json
import os
import platform
from pathlib import Path
import re
import shutil
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
BUILD = ROOT / ".routing-build"
quota_reads = set()
state_lock = threading.Lock()
fail_high = False


def test_backend():
    shipped = ROOT / "src/Sources/Resources/cli-proxy-api-plus"
    if platform.system() != "Darwin":
        return shipped
    # Go before 1.27 ignores SSL_CERT_FILE on macOS. Compile the exact pinned
    # backend source with Go 1.27 so TLS fixtures remain process-local. The
    # shipped backend is never overwritten and system trust is never changed.
    module = BUILD / "backend"
    module.mkdir(exist_ok=True)
    version = re.search(r"github.com/router-for-me/CLIProxyAPI/v8 (v\S+)", (ROOT / "routing-plugin/go.mod").read_text()).group(1)
    (module / "go.mod").write_text(f"module vibeproxy-routing-integration\n\ngo 1.27.0\n\nrequire github.com/router-for-me/CLIProxyAPI/v8 {version}\n")
    executable = module / "cli-proxy-api-test"
    subprocess.run(["go", "build", "-mod=mod", "-o", str(executable), "github.com/router-for-me/CLIProxyAPI/v8/cmd/server"], cwd=module, check=True)
    print(f"Using pinned CLIProxyAPI {version} source with Go 1.27 for process-local TLS fixtures", flush=True)
    return executable


class Provider(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, data, status=200, content_type="application/json"):
        raw = data.encode() if isinstance(data, str) else json.dumps(data).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        token = self.headers.get("Authorization", "").removeprefix("Bearer ")
        assert token.startswith("fixture-"), "Only synthetic fixture tokens are allowed"
        high = token.endswith("-high")
        remaining, days = (45, 3) if high else (6, 7)
        reset = int(time.time() + days * 86400)
        with state_lock:
            quota_reads.add(token)
        if self.path == "/api/oauth/usage":
            reset_iso = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(reset))
            self.reply({"five_hour": {"utilization": 10, "resets_at": reset_iso},
                        "seven_day": {"utilization": 100-remaining, "resets_at": reset_iso}})
        elif self.path == "/backend-api/wham/usage":
            assert self.headers.get("Chatgpt-Account-Id") == token
            self.reply({"rate_limit": {"allowed": True, "limit_reached": False,
                        "primary_window": {"used_percent": 10, "limit_window_seconds": 18000, "reset_after_seconds": 14400},
                        "secondary_window": {"used_percent": 100-remaining, "limit_window_seconds": 604800, "reset_at": reset}}})
        else:
            self.reply({"error": "Unexpected mock URL"}, 404)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        token = self.headers.get("Authorization", "").removeprefix("Bearer ")
        assert token.startswith("fixture-"), "Only synthetic fixture tokens are allowed"
        if fail_high and token.endswith("-high"):
            self.reply({"error": {"type": "rate_limit_error", "message": "Mock quota exceeded"}}, 429)
            return
        if self.path.startswith("/v1/messages"):
            response = {"id": "msg_fixture", "type": "message", "role": "assistant", "model": body["model"],
                        "content": [{"type": "text", "text": token}], "stop_reason": "end_turn", "stop_sequence": None,
                        "usage": {"input_tokens": 1, "output_tokens": 1}}
            if body.get("stream"):
                events = [
                    ("message_start", {"type": "message_start", "message": {**response, "content": [], "stop_reason": None}}),
                    ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
                    ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": token}}),
                    ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                    ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 1}}),
                    ("message_stop", {"type": "message_stop"}),
                ]
                self.reply("".join(f"event: {event}\ndata: {json.dumps(data)}\n\n" for event, data in events), content_type="text/event-stream")
            else:
                self.reply(response)
        elif self.path.startswith("/backend-api/codex/responses"):
            item = {"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed",
                    "content": [{"type": "output_text", "text": token, "annotations": []}]}
            response = {"id": "resp_fixture", "object": "response", "created_at": int(time.time()), "status": "completed",
                        "model": body["model"], "output": [item], "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
            events = [
                {"type": "response.created", "response": {**response, "status": "in_progress", "output": []}},
                {"type": "response.output_item.added", "output_index": 0, "item": {**item, "status": "in_progress", "content": []}},
                {"type": "response.content_part.added", "output_index": 0, "content_index": 0, "item_id": item["id"], "part": {"type": "output_text", "text": "", "annotations": []}},
                {"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": item["id"], "delta": token},
                {"type": "response.output_text.done", "output_index": 0, "content_index": 0, "item_id": item["id"], "text": token},
                {"type": "response.output_item.done", "output_index": 0, "item": item},
                {"type": "response.completed", "response": response},
            ]
            self.reply("".join(f"event: {event['type']}\ndata: {json.dumps(event)}\n\n" for event in events), content_type="text/event-stream")
        else:
            self.reply({"error": "Unexpected mock URL"}, 404)


class Proxy(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_CONNECT(self):
        if self.path not in ("api.anthropic.com:443", "chatgpt.com:443"):
            self.send_error(403)
            return
        self.send_response(200)
        self.end_headers()
        self.wfile.flush()
        self.close_connection = True
        with self.server.tls.wrap_socket(self.connection, server_side=True) as connection:
            Provider(connection, self.client_address, self.server)


def main():
    global fail_high
    BUILD.mkdir(exist_ok=True)
    backend = test_backend()
    with tempfile.TemporaryDirectory(prefix="routing-integration-", dir=BUILD) as temporary:
        fixture = Path(temporary)
        cert, key = fixture / "cert.pem", fixture / "key.pem"
        openssl_config = fixture / "openssl.cnf"
        openssl_config.write_text("[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=extensions\n[dn]\nCN=Routing mock\n[extensions]\nsubjectAltName=DNS:api.anthropic.com,DNS:chatgpt.com\nbasicConstraints=critical,CA:TRUE\n")
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-keyout", str(key), "-out", str(cert), "-config", str(openssl_config)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        mock = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Proxy)
        mock.tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        mock.tls.load_cert_chain(cert, key)
        threading.Thread(target=mock.serve_forever, daemon=True).start()
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        auth = fixture / "auth"
        auth.mkdir()
        plugins = fixture / "plugins"
        plugins.mkdir()
        shutil.copy2(ROOT / "src/Sources/Resources/Plugins/vibeproxy-quota.dylib", plugins)
        for provider in ("claude", "codex"):
            for suffix in ("low", "high"):
                token = f"fixture-{provider}-{suffix}"
                (auth / f"{provider}-{suffix}.json").write_text(json.dumps({"type": provider, "email": token+"@example.test", "access_token": token, "account_id": token, "expired": "2099-01-01T00:00:00Z"}))
        proxy_url = f"http://127.0.0.1:{mock.server_port}"
        config = fixture / "config.yaml"

        def write_config(enabled, minimums=None):
            content = f"host: 127.0.0.1\nport: {port}\nauth-dir: {json.dumps(str(auth))}\nproxy-url: {json.dumps(proxy_url)}\nrequest-retry: 0\nremote-management:\n  secret-key: ''\n  disable-control-panel: true\nrouting:\n  strategy: round-robin\nplugins:\n  enabled: true\n  dir: {json.dumps(str(plugins))}\n  configs:\n    vibeproxy-quota:\n      enabled: {str(enabled).lower()}\n      priority: 1\n      proxy-url: {json.dumps(proxy_url)}\n"

            if minimums is not None:
                content += "      minimum-remaining-percent: " + json.dumps(minimums) + "\n"
            pending = config.with_suffix(".pending")
            pending.write_text(content)
            pending.replace(config)

        write_config(True)
        environment = {**os.environ, "SSL_CERT_FILE": str(cert), "GODEBUG": "x509sslcertoverrideplatform=1"}
        log = BUILD / "routing-integration.log"
        with log.open("w") as output:
            process = subprocess.Popen([str(backend), "-config", str(config)], cwd=fixture, env=environment, stdout=output, stderr=output)
            client = urllib.request.build_opener(urllib.request.ProxyHandler({}))

            def call(path, payload=None):
                data = None if payload is None else json.dumps(payload).encode()
                headers = {"Content-Type": "application/json", "Authorization": "Bearer fixture-client"}
                if path == "/v1/messages":
                    headers["anthropic-version"] = "2023-06-01"
                request = urllib.request.Request(f"http://127.0.0.1:{port}"+path, data=data, headers=headers)
                try:
                    with client.open(request, timeout=12) as response:
                        return response.read().decode()
                except urllib.error.HTTPError as error:
                    raise AssertionError(f"Backend returned {error.code}: {error.read().decode()}") from error

            try:
                deadline = time.monotonic()+20
                while True:
                    try:
                        models = json.loads(call("/v1/models"))["data"]
                        if any(model["id"].startswith("claude-") for model in models) and any(model["id"].startswith("gpt-") for model in models):
                            break
                        if time.monotonic()>deadline:
                            raise AssertionError(f"Fixture account models did not register: {[model['id'] for model in models]}")
                        time.sleep(.1)
                    except urllib.error.URLError:
                        if process.poll() is not None or time.monotonic()>deadline:
                            raise AssertionError(f"Backend failed to start; inspect {log}")
                        time.sleep(.1)
                claude = next(model["id"] for model in models if model["id"].startswith("claude-") and "sonnet" in model["id"])
                codex = next(model["id"] for model in models if model["id"].startswith("gpt-") and not model["id"].startswith("gpt-image"))

                def infer(provider, stream=False):
                    if provider == "claude":
                        return call("/v1/messages", {"model": claude, "max_tokens": 20, "stream": stream, "messages": [{"role": "user", "content": "Routing fixture"}]})
                    return call("/v1/responses", {"model": codex, "stream": stream, "input": [{"role": "user", "content": "Routing fixture"}]})

                for provider in ("claude", "codex"):
                    infer(provider)  # Cold observation delegates while warming in the background.
                deadline = time.monotonic()+12
                while True:
                    with state_lock:
                        warmed = len(quota_reads) == 4
                    if warmed:
                        time.sleep(.2)  # let the parsed snapshots publish
                        break
                    if time.monotonic()>deadline:
                        raise AssertionError(f"Account-specific quota callbacks failed; inspect {log}")
                    time.sleep(.05)
                for provider in ("claude", "codex"):
                    for stream in (False, True):
                        for _ in range(2):
                            assert f"fixture-{provider}-high" in infer(provider, stream), f"Wrong {provider} account selected"
                    print(f"PASS {provider}: 45% / 3 days wins over 6% / 7 days, normal and streaming")

                write_config(False)
                deadline = time.monotonic()+12
                while True:
                    outputs = {provider: [infer(provider) for _ in range(4)] for provider in ("claude", "codex")}
                    if all(any(f"fixture-{provider}-low" in result for result in results) and
                           any(f"fixture-{provider}-high" in result for result in results)
                           for provider, results in outputs.items()):
                        break
                    if time.monotonic()>deadline:
                        raise AssertionError("Hot reload did not restore round robin")
                    time.sleep(.2)
                print("PASS switching quota mode off applies without restarting the backend")
                write_config(True)
                deadline = time.monotonic()+12
                while True:
                    if all(all(f"fixture-{provider}-high" in infer(provider) for _ in range(4))
                           for provider in ("claude", "codex")):
                        break
                    if time.monotonic()>deadline:
                        raise AssertionError("Hot reload did not re-enable quota routing")
                    time.sleep(.2)
                print("PASS switching quota mode on refreshes observations and restores quota routing")

                # Reserve the normally preferred accounts at exactly their 45%
                # remaining quota. Config changes must apply without a restart.
                write_config(True, {f"{p}-high.json": 45 for p in ("claude", "codex")})
                deadline = time.monotonic()+12
                while True:
                    if all(all(f"fixture-{provider}-low" in infer(provider, stream)
                               for stream in (False, True)) for provider in ("claude", "codex")):
                        break
                    if time.monotonic()>deadline:
                        raise AssertionError("Quota minimum failed to exclude the preferred account at its exact boundary")
                    time.sleep(.2)
                print("PASS per-account minimum hot reload excludes accounts at the exact boundary, normal and streaming")

                write_config(True, {f"{p}-{suffix}.json": 100 for p in ("claude", "codex") for suffix in ("low", "high")})
                deadline = time.monotonic()+12
                while True:
                    blocked = 0
                    for provider in ("claude", "codex"):
                        try:
                            infer(provider)
                        except AssertionError as error:
                            if "quota_exceeded" not in str(error):
                                raise
                            blocked += 1
                    if blocked == 2:
                        break
                    if time.monotonic()>deadline:
                        raise AssertionError("All-reserved pool fell back to a protected account")
                    time.sleep(.2)
                print("PASS all-reserved pools reject without backend fallback")

                write_config(True, {})
                deadline = time.monotonic()+12
                while True:
                    try:
                        if all(f"fixture-{provider}-high" in infer(provider) for provider in ("claude", "codex")):
                            break
                    except AssertionError as error:
                        if "quota_exceeded" not in str(error):
                            raise
                    if time.monotonic()>deadline:
                        raise AssertionError("Clearing minimums did not restore routing")
                    time.sleep(.2)
                print("PASS clearing minimums restores routing without restart")

                fail_high = True
                for provider in ("claude", "codex"):
                    assert f"fixture-{provider}-low" in infer(provider), f"{provider} failover did not choose eligible peer"
                    print(f"PASS {provider}: upstream 429 fails over to remaining eligible account")
                fail_high = False

                # Reload mode off and clear runtime cooldown by restarting only
                # this owned backend. This also tests shutdown with background work.
                process.terminate()
                process.wait(timeout=15)
                write_config(False)
                process = subprocess.Popen([str(backend), "-config", str(config)], cwd=fixture, env=environment, stdout=output, stderr=output)
                deadline = time.monotonic()+20
                while True:
                    try:
                        registered = {model["id"] for model in json.loads(call("/v1/models"))["data"]}
                        if {claude, codex}.issubset(registered):
                            break
                        if time.monotonic()>deadline:
                            raise AssertionError("Fixture account models did not register after restart")
                        time.sleep(.1)
                    except urllib.error.URLError:
                        if time.monotonic()>deadline: raise AssertionError("Restart failed")
                        time.sleep(.1)
                for provider in ("claude", "codex"):
                    outputs = [infer(provider) for _ in range(4)]
                    assert any(f"fixture-{provider}-low" in result for result in outputs)
                    assert any(f"fixture-{provider}-high" in result for result in outputs)
                print("PASS disabling quota mode restores round robin for both providers")
            finally:
                process.terminate()
                process.wait(timeout=15)
                mock.shutdown()


if __name__ == "__main__":
    main()

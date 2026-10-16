#!/usr/bin/env python3
"""Build the pinned backend with VibeProxy's loopback lockout fix.

Only task-owned build files are changed; the Go module cache and shipped upstream
binary are left intact. --test runs the management package's race-enabled tests.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--test", action="store_true")
    args = parser.parse_args()
    module = "github.com/router-for-me/CLIProxyAPI/v8"
    version = re.search(re.escape(module) + r" (v\S+)", (ROOT / "routing-plugin/go.mod").read_text()).group(1)
    metadata = json.loads(subprocess.check_output(
        ["go", "mod", "download", "-json", f"{module}@{version}"], cwd=ROOT / "routing-plugin"))
    expected = f"{module} {version} {metadata['Sum']}"
    if expected not in (ROOT / "routing-plugin/go.sum").read_text().splitlines():
        raise RuntimeError("Backend checksum does not match the pinned SDK")
    arch = os.environ.get("TARGET_ARCH", platform.machine())
    goarch = {"arm64": "arm64", "x86_64": "amd64"}[arch]
    build = ROOT / ".routing-build/management-backend" / arch
    build.mkdir(parents=True, exist_ok=True)
    patch = ROOT / "patches/management/localhost-no-ban.patch"
    patch_id = hashlib.sha256(patch.read_bytes()).hexdigest()[:12]
    with tempfile.TemporaryDirectory(prefix="source-", dir=build) as scratch:
        source = Path(scratch) / "backend"
        shutil.copytree(metadata["Dir"], source)
        source.chmod(0o755)
        for path in source.rglob("*"):
            path.chmod(path.stat().st_mode | 0o200)
        subprocess.run(["patch", "--batch", "--fuzz=0", "-p1", "-i", str(patch)], cwd=source, check=True)
        shutil.copy2(ROOT / "patches/management/local_access_test.go",
                     source / "internal/api/handlers/management/vibeproxy_local_access_test.go")
        if args.test:
            subprocess.run(["go", "test", "-race", "./internal/api/handlers/management"], cwd=source, check=True)
        env = dict(os.environ, GOARCH=goarch, CGO_ENABLED="1", MACOSX_DEPLOYMENT_TARGET="13.0")
        for name in ("CGO_CFLAGS", "CGO_LDFLAGS"):
            env[name] = env.get(name, "") + f" -arch {arch}"
        flags = f"-X main.Version={version[1:]}-vibeproxy-local -X main.Commit=local-auth-{patch_id}"
        output = build / "cli-proxy-api-plus"
        staged = Path(scratch) / output.name
        subprocess.run(["go", "build", "-trimpath", "-ldflags", flags, "-o", str(staged), "./cmd/server"],
                       cwd=source, env=env, check=True)
        staged.replace(output)
    print(f"Patched backend: {output}")


if __name__ == "__main__":
    main()

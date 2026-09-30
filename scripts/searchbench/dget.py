#!/usr/bin/env python3
"""Read-only Dispatch client: GET requests only.

The server and bearer come from DISPATCH_URL / DISPATCH_TOKEN, else from `dispatch.serverUrl` and
`dispatch.token` in ~/.config/opencode/envoy.json.

Usage as a CLI: dget.py <path-under-/api/v1> [--raw]  (prints JSON, or text with --raw)
"""

import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


def _config() -> tuple[str, str]:
    with open(os.path.expanduser("~/.config/opencode/envoy.json")) as f:
        cfg = json.load(f)["dispatch"]
    url = os.environ.get("DISPATCH_URL") or cfg["serverUrl"]
    tok = os.environ.get("DISPATCH_TOKEN") or cfg["token"]
    return url.rstrip("/") + "/api/v1", tok


_CFG = None


def get(path: str, params: dict | None = None, raw: bool = False):
    global _CFG
    if _CFG is None:
        _CFG = _config()
    base, token = _CFG
    url = base + path
    if params:
        url += "?" + urllib.parse.urlencode(params, doseq=True)
    req = urllib.request.Request(url, method="GET", headers={"Authorization": f"Bearer {token}"})
    for attempt in range(5):
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                body = resp.read().decode()
                return body if raw else json.loads(body)
        except urllib.error.HTTPError as e:
            if e.code in (429, 502, 503, 504) and attempt < 4:
                time.sleep(2**attempt)
                continue
            raise RuntimeError(f"GET {url} -> {e.code}: {e.read().decode()[:500]}") from e


if __name__ == "__main__":
    raw = "--raw" in sys.argv
    args = [a for a in sys.argv[1:] if a != "--raw"]
    path, _, query = args[0].partition("?")
    params = dict(urllib.parse.parse_qsl(query)) if query else None
    out = get(path, params, raw=raw)
    print(out if raw else json.dumps(out, indent=2))

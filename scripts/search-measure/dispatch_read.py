#!/usr/bin/env python3
"""The read-only Dispatch client the search-measure scripts share.

Every call here is a GET. The server is DISPATCH_URL and the bearer is the trimmed contents of
the file DISPATCH_TOKEN_FILE names (DISPATCH_TOKEN is read when the file is not set), so no URL
and no token is ever written into this repository.
"""

import json
import os
import queue
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


class ReadOnlyDispatch:
    """GET-only client of one Dispatch server."""

    def __init__(self, base=None, token=None, timeout=120.0):
        self.base = (base or os.environ["DISPATCH_URL"]).rstrip("/")
        self.token = token or _token()
        self.timeout = timeout

    def get(self, path, tries=5):
        """GET path and decode its JSON, retrying a transport error or a 5xx."""
        url = self.base + path
        last = None
        for attempt in range(tries):
            request = urllib.request.Request(
                url, headers={"Authorization": "Bearer " + self.token}, method="GET"
            )
            try:
                with urllib.request.urlopen(request, timeout=self.timeout) as answer:
                    return json.load(answer)
            except urllib.error.HTTPError as error:
                body = error.read()[:400].decode("utf-8", "replace")
                last = f"{error.code} {body}"
                if error.code < 500:
                    raise DispatchRefused(error.code, body) from error
            except Exception as error:  # noqa: BLE001 - a transport error is worth another try
                last = repr(error)
            time.sleep(1.0 * (attempt + 1))
        raise RuntimeError(f"GET {path}: {last}")

    def paged(self, path, key, limit=200):
        """Every row of a route that pages with limit and offset."""
        rows, offset = [], 0
        joiner = "&" if "?" in path else "?"
        while True:
            page = self.get(f"{path}{joiner}limit={limit}&offset={offset}")
            batch = page[key]
            rows.extend(batch)
            offset += len(batch)
            if not batch or offset >= page.get("total", 0):
                return rows


class DispatchRefused(Exception):
    """A 4xx: the route or the item is not there, and another try would not help."""

    def __init__(self, status, body):
        super().__init__(f"{status}: {body}")
        self.status = status
        self.body = body


def _token():
    path = os.environ.get("DISPATCH_TOKEN_FILE")
    if path:
        with open(path) as handle:
            return handle.read().strip()
    return os.environ["DISPATCH_TOKEN"].strip()


def pmap(function, items, workers=12, label=None, every=200):
    """function over items, workers at a time, answers in the order of items.

    An item whose call raises keeps its exception as the answer, so one unreachable issue never
    ends a harvest; the caller counts them.
    """
    answers = [None] * len(items)
    work = queue.Queue()
    for index in range(len(items)):
        work.put(index)
    done = [0]
    lock = threading.Lock()

    def worker():
        while True:
            try:
                index = work.get_nowait()
            except queue.Empty:
                return
            try:
                answers[index] = function(items[index])
            except Exception as error:  # noqa: BLE001 - kept as this item's answer
                answers[index] = error
            with lock:
                done[0] += 1
                if label and done[0] % every == 0:
                    print(f"  {label}: {done[0]}/{len(items)}", file=sys.stderr, flush=True)

    threads = [threading.Thread(target=worker, daemon=True) for _ in range(workers)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    return answers


def require_under_tmpdir(path):
    """Refuse an output directory outside $TMPDIR: a harvest of production is large, and this
    pod keeps everything large on the issue's own volume."""
    tmpdir = os.environ.get("TMPDIR")
    if not tmpdir:
        raise SystemExit("TMPDIR is not set: name the volume the corpus is written to")
    real = os.path.realpath(path)
    if not (real + os.sep).startswith(os.path.realpath(tmpdir) + os.sep):
        raise SystemExit(f"{path} is not under TMPDIR ({tmpdir})")
    return real

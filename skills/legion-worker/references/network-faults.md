# Making a download fail, and reading why one did

Part of `skill://legion-worker`. Read it before you build a rig that makes a download fail, before
you judge a retry a change adds, and when a download in your pod failed and you must say which part
of it failed. Every path it cites is in cybermonklabs/legion.

Every number below was measured in a Legion issue pod on 2026-11-07 (gVisor, uid 1000) with
curl 8.14.1, bun 1.4.4, gh 2.98.0, git 2.47.3, node v24.22.1 and go 1.27.1. A GitHub runner carries
other versions and other environment, so say which of the two a figure came from.

## The pod turns every proxy off, so a rig built on the proxy variables injects nothing

A Legion pod sets `no_proxy=*` and `NO_PROXY=*`:

```sh
printf 'no_proxy=[%s] NO_PROXY=[%s]\n' "$no_proxy" "$NO_PROXY"   # no_proxy=[*] NO_PROXY=[*]
```

`*` matches every host, so a client that reads those names ignores `https_proxy` completely and
reaches the real origin. With a proxy that answers 504 for `registry.npmjs.org` named in all four
proxy variables and `NO_PROXY` left as the pod sets it, `bun install` **succeeded** — `1 package
installed`, and the proxy logged no request at all. The same run with both names cleared failed.
curl, gh and git behave the same way.

So clear both for the one command, never for the shell:

```sh
env no_proxy= NO_PROXY= https_proxy=http://127.0.0.1:18080 <command>
```

A rig that skips this measures nothing, and its recovery run is green for the wrong reason.

## Which variable each client reads

Measured by counting the rig proxy's own log lines for one request of each client, one variable set
at a time, with `no_proxy` and `NO_PROXY` cleared.

| Client | Reads | Does not read |
| --- | --- | --- |
| `curl`, plain HTTP | `http_proxy` | `HTTP_PROXY` (curl ignores it for HTTP by design) |
| `curl`, HTTPS | `https_proxy`, `HTTPS_PROXY` | — |
| `bun` 1.4.4 | `https_proxy`, `HTTPS_PROXY` | — |
| `gh` 2.98.0 | `https_proxy`, `HTTPS_PROXY` | — |
| `git` 2.47.3 | `https_proxy` | `HTTPS_PROXY` alone: the request went direct |
| `node` 24 `fetch` | `https_proxy`, **and only** with `NODE_USE_ENV_PROXY=1` | `HTTPS_PROXY`, with or without that variable; either name without it |

Set the whole set — `http_proxy`, `https_proxy`, `HTTP_PROXY`, `HTTPS_PROXY` and
`NODE_USE_ENV_PROXY=1` — unless the point of the run is which name a client reads.

`apt` is not on this list: a pod cannot run it. `apt-get update` as uid 1000 ends with
`E: List directory /var/lib/apt/lists/partial is missing. - Acquire (13: Permission denied)`.

## The rig

A CONNECT proxy that answers 504 for the hosts you name and tunnels every other host, and that logs
one line per request so a run's attempts can be counted. Put it under `$TMPDIR`, never in the
workspace:

```python
#!/usr/bin/env python3
"""proxy.py <port> <logfile> [--fail HOST]... [--stall HOST]..."""
import socket, sys, threading, time

port, logpath = int(sys.argv[1]), sys.argv[2]
fail, stall, mode = set(), set(), None
for arg in sys.argv[3:]:
    if arg in ("--fail", "--stall"):
        mode = fail if arg == "--fail" else stall
    else:
        mode.add(arg)
lock = threading.Lock()

def log(line):
    with lock, open(logpath, "a") as fh:
        fh.write("%.3f %s\n" % (time.time(), line))

def pump(a, b):
    try:
        while True:
            data = a.recv(65536)
            if not data:
                break
            b.sendall(data)
    except OSError:
        pass

def handle(client):
    try:
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = client.recv(65536)
            if not chunk:
                return
            buf += chunk
        head, rest = buf.split(b"\r\n\r\n", 1)
        verb, target = head.decode("latin-1").split("\r\n")[0].split(" ")[:2]
        if verb == "CONNECT":
            host, _, p = target.partition(":")
            upstream_port = int(p or 443)
        else:
            authority = target.split("://", 1)[-1].split("/")[0]
            host, upstream_port = authority.split(":")[0], int(authority.partition(":")[2] or 80)
        if host in stall:
            log("STALL %s %s" % (verb, host))
            while True:
                time.sleep(3600)
        if host in fail:
            log("FAIL504 %s %s" % (verb, host))
            client.sendall(b"HTTP/1.1 504 Gateway Timeout\r\nContent-Length: 11\r\n"
                           b"Connection: close\r\n\r\n504 refused")
            return
        log("PASS %s %s" % (verb, host))
        upstream = socket.create_connection((host, upstream_port), timeout=30)
        if verb == "CONNECT":
            client.sendall(b"HTTP/1.1 200 Connection established\r\n\r\n")
        else:
            upstream.sendall(buf)
            rest = b""
        if rest:
            upstream.sendall(rest)
        threading.Thread(target=pump, args=(client, upstream), daemon=True).start()
        pump(upstream, client)
    except Exception as exc:
        log("ERROR %s" % exc)
    finally:
        client.close()

server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
server.bind(("127.0.0.1", port))
server.listen(64)
print("listening on 127.0.0.1:%d" % port, flush=True)
while True:
    conn, _ = server.accept()
    threading.Thread(target=handle, args=(conn,), daemon=True).start()
```

```sh
nohup python3 "$TMPDIR/rig/proxy.py" 18080 "$TMPDIR/rig/fail.log" --fail api.github.com &
```

Count a run's attempts from the log rather than from the client's own words: the line count before
and after the run is the number of requests that reached the proxy, and the first and last stamps
are the span the client spent retrying.

## A rig that faults CONNECT tests a different path from a real 504

A 504 the origin sends is an HTTP response. A 504 the rig sends to `CONNECT` is a tunnel that never
opened, and the two go down different paths in the same client. Measured with curl, where the
difference decides whether a retry runs at all:

| What answers 504 | `curl` with no `--retry` | `curl --retry 3` | `curl --retry 3 --retry-all-errors` |
| --- | --- | --- | --- |
| the origin, as a response | 1 attempt, **exit 0**, `%{http_code}` 504 | 4 attempts, 7.07 s, **exit 0** | 4 attempts, exit 0 |
| the proxy, to `CONNECT` | 1 attempt, exit 56 | **1 attempt**, exit 56 | 4 attempts, 7.04 s, exit 56 |

Two things follow for a check that judges a retry.

`--retry` alone does not retry a failed tunnel: `curl: (56) CONNECT tunnel failed, response 504`
ends the command at the first attempt. A rig that faults CONNECT therefore reports "no retry" for a
step whose `--retry 3` would have retried the real fault. Say which of the two a result measured, or
fault the origin.

`curl --retry 3` **exits 0 on a 504 it never got past**, with the error body written to the output
file. A step that reads only the exit status treats a minute of 504s as a success and installs
whatever the body was. `--fail` (or `-f`), or a read of `%{http_code}`, is what turns it into a
failure.

```sh
curl -sS --retry 3 --max-time 10 -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18090/   # 504, exit 0
```

## `--max-time` is per attempt, never a bound on the retries

Against a proxy that accepts the connection and answers nothing:

```sh
env no_proxy= NO_PROXY= https_proxy=http://127.0.0.1:18081 HTTPS_PROXY=http://127.0.0.1:18081 \
  curl -sS --retry 3 --retry-all-errors --max-time 5 -o /dev/null https://api.github.com/
```

4 attempts of 5.00 s each, with waits of 1 s, 2 s and 4 s between them: **27.06 s** in all, exit 28,
`Connection timed out after 5005 milliseconds`. A step that sets `--max-time 5` to bound itself at
five seconds is bounded at `(n+1) × max-time` plus the backoff. `--retry-max-time` is the bound on
the whole sequence.

## What each client does on its own, with no retry added

- **bun 1.4.4 retries, for under two seconds.** With `registry.npmjs.org` answering 504, `bun
  install` made **6 attempts spanning 1.67 s and 1.73 s** over two runs, the waits growing from
  0.06 s to 0.87 s, then stopped with `error: ProxyConnectFailed (504) downloading package
  manifest`. So bun is not without a retry, and its retry is three orders of magnitude short of the
  minute of 504s a CI lane must survive. Read the proxy log, not the error line: the error says
  nothing about how long bun tried.
- **gh 2.98.0 does not retry.** One request, then `Get "https://api.github.com/rate_limit": Gateway
  Timeout`.
- **git 2.47.3 does not retry a failed tunnel.** One request, then `fatal: unable to access …:
  CONNECT tunnel failed, response 504`.

## The Go command reads the image's own module cache first

A pod's `GOPROXY` is `file:///home/legion/go/pkg/mod/cache/download|https://proxy.golang.org|direct`
over a 387 MB cache baked into the worker image (`go env GOPROXY`, `du -sh` of that path). Two
consequences for a fault rig:

- A module the image holds never reaches the network. `go get github.com/pkg/errors@v0.9.1`
  succeeded with `proxy.golang.org`, `sum.golang.org` and `github.com` all answering 504.
- A module it does not hold goes to `proxy.golang.org` (4 attempts, all 504), falls through on the
  `|` separator to `direct`, and dies there in git:
  `go: github.com/mikefarah/yq/v4@v4.44.3: invalid version: git ls-remote … CONNECT tunnel failed,
  response 504`.

The checksum database is a third host, and faulting the module proxy alone leaves it answering: with
`proxy.golang.org` faulted and `sum.golang.org` reachable, the module downloaded through `direct`
and the build went on; with both faulted, it downloaded and then failed at
`verifying module: … Get "https://sum.golang.org/lookup/…": Gateway Timeout`. Fault all three, and
name a module the image's cache does not hold, or the run proves nothing.

## `make` is absent, so an install that builds from source is no evidence about a download

`command -v make` prints nothing in a pod, while `cc` and `gcc` are both there, and `apt-get` cannot
install one (uid 1000). A dependency whose install falls back to `node-gyp` therefore fails for a
reason that is not the thing under test:

```sh
cd node_modules/better-sqlite3 && bunx --package node-gyp@11.4.2 node-gyp rebuild
# gyp ERR! stack Error: not found: make
```

So the exit status of an install that reaches a native build answers no question about whether a
download was retried or served from the tree. Judge such a run by what it printed and fetched — the
download lines, the proxy's log, the installed files, the lockfile's `sha512` — and run the install
a second time with `--ignore-scripts` to show that nothing else is missing. The full install on a
runner, which has a toolchain, is the pull request's own CI lane.

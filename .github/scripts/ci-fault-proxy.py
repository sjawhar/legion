#!/usr/bin/env python3
"""An HTTP proxy that injects the download fault this repository's retries answer.

It serves plain HTTP (an absolute-URI request line) and HTTPS (CONNECT), because a fetch of a
tool reaches a proxy as either one: curl of a release asset over https, `bun install` of a
registry, `go mod download` of a module proxy, apt over http. Three modes:

  fail     answer HTTP 504 to every request for a named host, for the whole run
  recover  answer HTTP 504 for --recover-after seconds, then pass the request on
  stall    accept the connection and send nothing at all, so each attempt of the client ends at
           its own time bound rather than at an answer

A host the --hosts list does not name is passed on in every mode, so a step can fault the host
under test and still reach the rest of the network — the Actions service among it, which a job
that cannot report is no proof of anything. An empty --hosts faults every host.

    .github/scripts/ci-fault-proxy.py --mode recover --recover-after 60 \\
        --hosts registry.npmjs.org --port 0 --port-file "$RUNNER_TEMP/proxy.port"

--port 0 takes a free port from the kernel and --port-file writes it, so a workflow step starts
the proxy, reads the port and sets http_proxy and https_proxy for the step under test. The file
is written whole, by rename, so a reader never sees half a port; the recovery window starts the
moment it appears, which is the first moment a caller can use the proxy.

--bind is the address it listens on, 127.0.0.1 by default. A `RUN` of a docker build runs in a
network namespace of its own, where 127.0.0.1 is the build container and not the runner, so a
build reaches this proxy only on an address the container can route to: the docker bridge
gateway, which .github/workflows/ci.yaml's download-retry-images job binds to and passes into
the build as http_proxy. Nothing else changes: a caller that names no --bind listens where it
always did.

It makes no fetch of its own: every byte it sends upstream is one a client sent it. It is Python
3 from the runner image, with nothing to install.
"""

from __future__ import annotations

import argparse
import os
import select
import socket
import socketserver
import sys
import time

READ_SIZE = 65536
HEADER_LIMIT = 65536


def log(message: str) -> None:
    sys.stderr.write(f"ci-fault-proxy: {message}\n")
    sys.stderr.flush()


class Fault:
    """Which hosts fail, how they fail, and until when."""

    def __init__(self, mode: str, hosts: list[str], recover_after: float) -> None:
        self.mode = mode
        self.hosts = [h.lower() for h in hosts]
        self.recover_after = recover_after
        self.started = time.monotonic()

    def faults(self, host: str) -> bool:
        host = host.lower()
        if self.hosts and not any(host == h or host.endswith("." + h) for h in self.hosts):
            return False
        if self.mode == "recover":
            return (time.monotonic() - self.started) < self.recover_after
        return True


class Handler(socketserver.StreamRequestHandler):
    # No read buffer: this handler reads the request line and the headers off the socket and then
    # hands the rest of the connection to pipe(), which reads the socket directly. A buffered
    # rfile would have swallowed whatever arrived in the same packet as the headers — a POST body,
    # or the TLS ClientHello a client may send right after CONNECT — and pipe() would never see it.
    rbufsize = 0
    timeout = 300
    fault: Fault
    stall_seconds: float

    def handle(self) -> None:
        try:
            request_line = self.rfile.readline(HEADER_LIMIT)
        except OSError:
            return
        if not request_line:
            return
        try:
            method, target, _version = request_line.decode("latin-1").split()
        except ValueError:
            self.answer(400, b"ci-fault-proxy: malformed request line\n")
            return
        headers = self.read_headers()
        if headers is None:
            self.answer(400, b"ci-fault-proxy: malformed headers\n")
            return
        try:
            if method.upper() == "CONNECT":
                self.do_connect(target)
            else:
                self.do_plain(method, target, headers)
        except ValueError as error:
            # A port that is not a number, and anything else a malformed target makes of the
            # parsing below: an answer, not a traceback from a thread nobody is reading.
            self.answer(400, f"ci-fault-proxy: {error}\n".encode())

    def read_headers(self) -> list[bytes] | None:
        headers: list[bytes] = []
        while True:
            line = self.rfile.readline(HEADER_LIMIT)
            if not line:
                return None
            if line in (b"\r\n", b"\n"):
                return headers
            headers.append(line)
            if len(headers) > 200:
                return None

    def answer(self, status: int, body: bytes) -> None:
        reason = {400: "Bad Request", 502: "Bad Gateway", 504: "Gateway Timeout"}.get(status, "Error")
        head = (
            f"HTTP/1.1 {status} {reason}\r\n"
            f"Content-Length: {len(body)}\r\n"
            "Content-Type: text/plain\r\n"
            "Connection: close\r\n\r\n"
        ).encode("latin-1")
        try:
            self.wfile.write(head + body)
            self.wfile.flush()
        except OSError:
            pass

    def stall(self, host: str, what: str) -> None:
        """Hold the connection open and send nothing, until the client gives up or the run ends."""
        log(f"stall {what} {host}")
        deadline = time.monotonic() + self.stall_seconds
        while time.monotonic() < deadline:
            ready, _, _ = select.select([self.connection], [], [], 1.0)
            if ready and not self.connection.recv(READ_SIZE):
                return

    @staticmethod
    def split_authority(authority: str, default_port: int) -> tuple[str, int]:
        if authority.startswith("["):
            host, _, rest = authority[1:].partition("]")
            port = int(rest[1:]) if rest.startswith(":") else default_port
            return host, port
        host, _, port = authority.partition(":")
        return host, int(port) if port else default_port

    def injected(self, host: str, label: str, what: str) -> bool:
        """Whether the fault answered this request itself: a stall that held it, or a 504."""
        if not self.fault.faults(host):
            return False
        if self.fault.mode == "stall":
            self.stall(host, label)
            return True
        log(f"504 {what}")
        self.answer(504, b"ci-fault-proxy: injected fault\n")
        return True

    def connect_upstream(self, host: str, port: int, what: str) -> socket.socket | None:
        """The connection to the origin, or None once a 502 has answered for its refusal."""
        try:
            upstream = socket.create_connection((host, port), timeout=30)
        except OSError as error:
            log(f"502 {what}: {error}")
            self.answer(502, f"ci-fault-proxy: {error}\n".encode())
            return None
        log(f"pass {what}")
        return upstream

    def do_connect(self, authority: str) -> None:
        host, port = self.split_authority(authority, 443)
        what = f"CONNECT {host}:{port}"
        if self.injected(host, "CONNECT", what):
            return
        upstream = self.connect_upstream(host, port, what)
        if upstream is None:
            return
        try:
            self.wfile.write(b"HTTP/1.1 200 Connection established\r\n\r\n")
            self.wfile.flush()
            self.pipe(upstream)
        finally:
            upstream.close()

    def do_plain(self, method: str, target: str, headers: list[bytes]) -> None:
        if not target.lower().startswith("http://"):
            self.answer(400, b"ci-fault-proxy: an absolute URI is required\n")
            return
        rest = target[len("http://") :]
        authority, slash, path = rest.partition("/")
        path = "/" + path if slash else "/"
        host, port = self.split_authority(authority, 80)
        what = f"{method} http://{authority}{path}"
        if self.injected(host, method, what):
            return
        # The hop-by-hop headers go, and the connection is a single exchange: this proxy keeps no
        # pool, so an origin left holding the connection open would only make pipe() wait for its
        # own timeout after the client already has the whole answer.
        kept = [h for h in headers if not h.lower().startswith((b"proxy-", b"connection:", b"keep-alive:"))]
        upstream = self.connect_upstream(host, port, what)
        if upstream is None:
            return
        try:
            head = f"{method} {path} HTTP/1.1\r\n".encode("latin-1") + b"".join(kept) + b"Connection: close\r\n\r\n"
            upstream.sendall(head)
            self.pipe(upstream)
        finally:
            upstream.close()

    def pipe(self, upstream: socket.socket) -> None:
        downstream = self.connection
        sockets = [downstream, upstream]
        while True:
            ready, _, bad = select.select(sockets, [], sockets, 60)
            if bad or not ready:
                return
            for source in ready:
                sink = upstream if source is downstream else downstream
                try:
                    chunk = source.recv(READ_SIZE)
                except OSError:
                    return
                if not chunk:
                    return
                try:
                    sink.sendall(chunk)
                except OSError:
                    return


class Proxy(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def handle_error(self, request, client_address) -> None:
        """One line, where the base class prints a traceback: a bug in this program must read as
        a bug in it, not as the fault it injects."""
        error = sys.exc_info()[1]
        log(f"handler failed: {type(error).__name__}: {error}")


def write_port_file(path: str, port: int) -> None:
    """The port, written whole: a reader polling for the file never reads half of a number."""
    temporary = f"{path}.{os.getpid()}.tmp"
    with open(temporary, "w", encoding="utf-8") as handle:
        handle.write(f"{port}\n")
    os.replace(temporary, path)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--mode", choices=("fail", "recover", "stall"), required=True)
    parser.add_argument("--hosts", default="", help="comma-separated hosts to fault; empty faults every host")
    parser.add_argument("--recover-after", type=float, default=60.0,
                        help="seconds of 504s before --mode recover passes requests on, counted from the port file")
    parser.add_argument("--stall-seconds", type=float, default=600.0,
                        help="how long --mode stall holds one connection open before dropping it")
    parser.add_argument("--bind", default="127.0.0.1",
                        help="the address to listen on; a docker build reaches it on the bridge gateway, not on 127.0.0.1")
    parser.add_argument("--port", type=int, default=0, help="0 takes a free port from the kernel")
    parser.add_argument("--port-file", default="", help="where to write the port it listens on")
    args = parser.parse_args(argv)

    hosts = [h.strip() for h in args.hosts.split(",") if h.strip()]
    Handler.stall_seconds = args.stall_seconds
    server = Proxy((args.bind, args.port), Handler)
    port = server.server_address[1]
    if args.port_file:
        write_port_file(args.port_file, port)
    # The clock of --mode recover starts here, after the port is announced and before a single
    # request is served: between bind and serve_forever a connection waits in the backlog, so no
    # request can be answered out of the window it belongs to.
    Handler.fault = Fault(args.mode, hosts, args.recover_after)
    log(f"mode={args.mode} bind={args.bind} port={port} hosts={','.join(hosts) or 'every host'} pid={os.getpid()}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())

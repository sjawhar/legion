#!/usr/bin/env python3
"""held-port.py: start a server on a port the OS chose, held from the choice until it serves.

usage:
  held-port.py exec [--host H | --from SOCKPATH] [--port-file F] --fd-env NAME
                    [--env K=V]... -- CMD [ARG]...
  held-port.py keep [--host H] --port-file F --socket SOCKPATH

exec
  Opens a listening TCP socket on H:0 (default 127.0.0.1; an IPv6 address such as ::1 works),
  so the kernel chooses the port, or with --from receives one already listening from the keeper
  serving the unix socket SOCKPATH. It puts the socket on descriptor 3, inheritable, and closes
  every other copy it holds. It replaces {port} in each --env value and in CMD and its ARGs
  with the socket's port, sets each --env K=V and NAME=3 in the environment, writes the port
  and a newline to F (atomically: a temporary file in F's directory, then a rename) and executes
  CMD in its own process. The PID stays the same, so a shell's $! is the server's PID.

  CMD must adopt the socket NAME names (ENVOY_LISTEN_FD, DISPATCH_LISTEN_FD, LEGION_LISTEN_FD):
  a server that binds the port itself fails with "address already in use".

  Any failure before CMD runs exits non-zero, naming what failed on stderr, and the socket closes
  with the process. A CMD that cannot be executed exits 127 (not found) or 126 after F was
  written; the port is free again once the process exits.

keep
  Opens one listening TCP socket on H:0 and holds it for its whole life. It serves the unix
  socket SOCKPATH, answering each connection with a copy of the held socket (SCM_RIGHTS), then
  writes the port to F, so a reader of F can connect at once. Each server started with
  `exec --from SOCKPATH` serves the same port, and a restart cannot lose it to another process.
  It runs until killed: SIGTERM or SIGINT exits 0 and removes SOCKPATH.

  Between servers the kept port still listens: the kernel accepts a connection into the queue,
  and nothing answers it until the next server starts and accepts it. A caller that checks
  whether the server is down sees a hang, not a refusal, so it polls with a timeout (for
  example `curl --max-time 2`) and takes the timeout for down.

This replaces picking a free port and passing its number: a port that was free a moment ago can
be taken before the server binds it, and a held one cannot.
"""

import argparse
import os
import signal
import socket
import sys

FD = 3
USAGE = __doc__.split("\n\n", 2)[1]


class Refusal(Exception):
    """A failure the helper reports on stderr and exits 1 for."""


def listen(host):
    """Return a socket listening on host and a port the kernel chooses."""
    try:
        family, kind, proto, _, address = socket.getaddrinfo(
            host, 0, type=socket.SOCK_STREAM, flags=socket.AI_PASSIVE
        )[0]
        held = socket.socket(family, kind, proto)
    except OSError as error:
        raise Refusal(f"open a TCP socket on {host}: {error}") from error
    try:
        held.bind(address)
        held.listen(socket.SOMAXCONN)
    except OSError as error:
        held.close()
        raise Refusal(f"listen on {host} port 0: {error}") from error
    return held


def receive(path):
    """Return the listening socket the keeper serving the unix socket path hands over."""
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as keeper:
            keeper.connect(path)
            # Close-on-exec from the moment it arrives, so no copy but descriptor 3 reaches CMD.
            _, fds, flags, _ = socket.recv_fds(keeper, 1, 1, socket.MSG_CMSG_CLOEXEC)
    except OSError as error:
        raise Refusal(f"receive the held socket from the keeper at {path}: {error}") from error
    if len(fds) != 1 or flags & socket.MSG_CTRUNC:
        for fd in fds:
            os.close(fd)
        raise Refusal(f"the keeper at {path} sent {len(fds)} descriptors, not one socket")
    try:
        held = socket.socket(fileno=fds[0])
    except OSError as error:
        os.close(fds[0])
        raise Refusal(f"the keeper at {path} sent no socket: {error}") from error
    if (
        held.family not in (socket.AF_INET, socket.AF_INET6)
        or held.type != socket.SOCK_STREAM
        or held.getsockopt(socket.SOL_SOCKET, socket.SO_ACCEPTCONN) != 1
    ):
        held.close()
        raise Refusal(f"the keeper at {path} sent a socket that is not a listening TCP socket")
    return held


def port_of(held):
    return held.getsockname()[1]


def write_port(path, port):
    """Write port to path through a temporary file in its directory and a rename."""
    temporary = os.path.join(
        os.path.dirname(os.path.abspath(path)), f".{os.path.basename(path)}.{os.getpid()}.tmp"
    )
    try:
        with open(temporary, "w", encoding="ascii") as file:
            file.write(f"{port}\n")
        os.replace(temporary, path)
    except OSError as error:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise Refusal(f"write the port file {path}: {error}") from error


def environment_pair(value):
    key, separator, _ = value.partition("=")
    if not separator or not key:
        raise argparse.ArgumentTypeError(f"--env takes K=V, not {value!r}")
    return value


def run_exec(args, command):
    if not command:
        raise Refusal("exec takes a command after --")
    if not args.fd_env or "=" in args.fd_env:
        raise Refusal(f"--fd-env takes a variable name, not {args.fd_env!r}")
    held = receive(args.source) if args.source else listen(args.host)
    port = port_of(held)
    fd = held.detach()
    if fd == FD:
        os.set_inheritable(FD, True)
    else:
        os.dup2(fd, FD, inheritable=True)
        os.close(fd)

    def substitute(text):
        return text.replace("{port}", str(port))

    env = dict(os.environ)
    for pair in args.env:
        key, _, value = pair.partition("=")
        env[key] = substitute(value)
    env[args.fd_env] = str(FD)
    argv = [substitute(word) for word in command]
    if args.port_file:
        write_port(args.port_file, port)
    # Python ignores these two at start, and an ignored signal stays ignored across exec.
    signal.signal(signal.SIGPIPE, signal.SIG_DFL)
    signal.signal(signal.SIGXFSZ, signal.SIG_DFL)
    try:
        os.execvpe(argv[0], argv, env)
    except OSError as error:
        print(f"held-port: execute {argv[0]}: {error}", file=sys.stderr)
        sys.exit(127 if isinstance(error, FileNotFoundError) else 126)


def stop(signum, frame):
    raise SystemExit(0)


def run_keep(args):
    held = listen(args.host)
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        server.bind(args.socket)
    except OSError as error:
        raise Refusal(f"serve the unix socket {args.socket}: {error}") from error
    try:
        try:
            server.listen(socket.SOMAXCONN)
        except OSError as error:
            raise Refusal(f"serve the unix socket {args.socket}: {error}") from error
        write_port(args.port_file, port_of(held))
        while True:
            connection, _ = server.accept()
            with connection:
                try:
                    socket.send_fds(connection, [b"\0"], [held.fileno()])
                except OSError as error:
                    print(f"held-port: hand the held socket to a server: {error}", file=sys.stderr)
    finally:
        server.close()
        try:
            os.unlink(args.socket)
        except FileNotFoundError:
            pass


def parser(name):
    commands = argparse.ArgumentParser(prog=f"held-port.py {name}", add_help=False, allow_abbrev=False)
    commands.add_argument("--port-file", required=name == "keep")
    if name == "exec":
        source = commands.add_mutually_exclusive_group()
        source.add_argument("--host", default="127.0.0.1")
        source.add_argument("--from", dest="source")
        commands.add_argument("--fd-env", required=True)
        commands.add_argument("--env", action="append", default=[], type=environment_pair)
    else:
        commands.add_argument("--host", default="127.0.0.1")
        commands.add_argument("--socket", required=True)
    return commands


def main(argv):
    if argv[:1] in (["-h"], ["--help"]):
        print(__doc__, end="")
        return 0
    if argv[:1] not in (["exec"], ["keep"]):
        print(USAGE, file=sys.stderr)
        return 2
    name, rest = argv[0], argv[1:]
    options, command = rest, []
    if name == "exec" and "--" in rest:
        split = rest.index("--")
        options, command = rest[:split], rest[split + 1 :]
    if "-h" in options or "--help" in options:
        print(__doc__, end="")
        return 0
    args = parser(name).parse_args(options)
    try:
        if name == "exec":
            run_exec(args, command)
        else:
            run_keep(args)
    except Refusal as refusal:
        print(f"held-port: {refusal}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

---
title: "A binary that serves an inherited socket checks it, adopts it, and judges its real address"
category: envoy
tags:
  - listenfd
  - inherited-descriptor
  - close-on-exec
  - scm-rights
  - docker
date: 2026-10-27
status: active
module: packages/envoy/listenfd
related_issues:
  - "INFRA-253"
  - "sjawhar/legion#2229"
---

# A binary that serves an inherited socket checks it, adopts it, and judges its real address

## Rule

- Reuse `packages/envoy/listenfd` (public, so the daemon's module can import it): `Inspect`,
  `Socket.CheckAgainst`, `Adopt`. Do not write a second reader.
- Before serving, check that the descriptor is open, a stream socket, IPv4 or IPv6, and listening.
  `SO_ACCEPTCONN` is non-zero when listening, not 1: darwin answers the option's bit. A Python
  reader tests `== 0` too, and passes `MSG_CMSG_CLOEXEC` only where `socket` defines it (darwin
  does not), setting close-on-exec itself otherwise.
- A host or port setting that is set must name the socket's address; refuse, naming both, when it
  does not. Never fall back to binding. Port 0 never names a socket.
- Every address-based check judges the socket's real address, never a default the settings would
  build: the listener's unauthenticated-API fence and Dispatch's dev sign-in fence both refuse an
  inherited socket on every interface.
- Inspect early, adopt once: only `Adopt` closes the raw descriptor, and it serves on a
  close-on-exec copy. A process that runs children before it serves (the daemon's key commands)
  adopts first, then unsets the variable, so no child inherits fd 3 or a stale variable naming it.
- A command started under `env -i` names the descriptor variable itself (`X_LISTEN_FD=3`); the
  helper's variable does not survive the wipe.
- Prove it by the socket's holders, not by fd 3: read the listening inode from `/proc/net/tcp`
  and the holders from `/proc/<pid>/fd`. Assert exactly the expected holders and close-on-exec in
  `fdinfo`. The Go runtime reuses fd 3 at once once it is free, so "fd 3 is closed" flakes.
- Docker cannot pass a host descriptor into a container. For a host-network container, serve the
  socket from the test over an abstract unix socket (the network namespace scopes abstract names,
  and a host-network container shares it) and run a wrapper as the entrypoint that receives it by
  SCM_RIGHTS, `dup3`s it onto fd 3 without close-on-exec, and execs the real server in its place.
  The wrapper uses raw syscalls before the exec, since package `net` starts the poller, which must
  not land on fd 3. A container restart fetches a fresh copy.

## Evidence

- `packages/envoy/listenfd/listenfd.go` and `inspect_unix.go`; callers
  `packages/envoy/cmd/dispatch/main.go` (`resolveListen`), `packages/envoy/internal/config/config.go`
  (`Load`, before `validateListenerAPIAuth`), `packages/daemon/cmd/legion/main.go`
  (`adoptListenFD`, before `config.Load`).
- `packages/envoy/infra/host/heldportwrapper/main.go` and `internal/pulumitest/heldport.go`;
  envoy-smoke's `docker restart` check in `packages/envoy/infra/host/live_test.go` passed at
  bd64de98 (run 40302789136).
- The holder check is `rig.sh`'s `listening_holders` and `held_only_by`; Stage 1 fails when a
  process other than the daemon and its keeper holds the socket.
- Review of #2229 found the Python `--from` path broke on darwin (`SO_ACCEPTCONN != 1`, no
  `MSG_CMSG_CLOEXEC`); fixed in 9729362306a9.

---
title: "A C compiler in a Go image turns cgo on: hold the default in ENV and $GOROOT/go.env, and check it in the build"
category: daemon
tags:
  - worker-image
  - dockerfile
  - go
  - cgo
  - race-detector
  - go-env
date: 2026-10-26
status: active
module: packages/daemon/docker
related_issues:
  - "INFRA-257"
  - "sjawhar/legion#2222"
---

# A C compiler in a Go image turns cgo on: hold the default in ENV and $GOROOT/go.env, and check it in the build

Stock Go turns cgo on when `gcc` (CC's default) is on `PATH`. An image that adds a C compiler and
must keep cgo off by default needs two settings, since each covers places the other misses:

- **`ENV CGO_ENABLED=0`:** every process that inherits the image's environment gets it,
  including:
  - a `go/build` library reader such as go/types' source importer, whose `defaultContext` reads
    only `os.Getenv` and never reads `go.env`;
  - a toolchain Go switches to for `GOTOOLCHAIN` or a `go.mod` `toolchain` line. That toolchain
    has its own GOROOT and `go.env`, but it keeps the environment.
- **`CGO_ENABLED=0` appended to `$GOROOT/go.env`:** this holds in a scrubbed environment, such as
  `env -i`, Oh My Pi's Python eval kernel, or a test that builds an environment of its own.
  cmd/go reads the process environment first, then the `go env -w` file, then `$GOROOT/go.env`.
- **What still gets Go's own default (cgo on):** a scrubbed environment that also switches
  toolchain, and a `go/build` reader under a scrubbed environment. With the compiler present those
  builds work. Document both cases rather than engineer around them.
- **Opt-in:** a role turns cgo on per command with `CGO_ENABLED=1`. `go env -w CGO_ENABLED=1`
  does not override the ENV; Go prints a warning and keeps the environment's value. An empty
  `CGO_ENABLED=` counts as unset.
- **Build check:** a property that one appended line holds gets a `test` in the image's existing
  toolchain step, written as the scrubbed case sees it:
  `test "$(env -u CGO_ENABLED go env CGO_ENABLED)" = 0`, run with `XDG_CONFIG_HOME` in scratch so
  no `go env -w` file is read. If the plan says "only the live proof catches X regressing", that
  is the line to add.
- **Read a binary, not the environment:** `go version -m <bin> | grep CGO_ENABLED` shows the
  setting the build actually used.
- **Race canary:** build it with `go build -race` and run the binary. `go run` does not pass the
  detector's exit status 66 through.
- **Packages:** under `--no-install-recommends`, name `libc6-dev` beside `gcc`. On trixie it is
  only a Recommends of gcc-14, and cgo builds against its libc headers and startup files.
- **Comments:** before a comment gives a reason for a mechanism, measure the reason again on the
  head image. A failure measured on the old image may not happen once the change is in.

## Evidence

- **Sources read:** the INFRA-257 plan read Go 1.27.1's `cmd/go/internal/cfg/cfg.go`
  (CGO_ENABLED via `cfg.Getenv`), `go/build/build.go` `defaultContext`, and
  `cmd/go/internal/toolchain/exec.go`.
- **Proof checks on PR #2222's head image:**
  - `env -i … go env CGO_ENABLED` printed `0` (from go.env).
  - `GOTOOLCHAIN=go1.26.8 go env GOVERSION CGO_ENABLED` printed `go1.26.8 0` (from the ENV).
  - The supervise table test, a go/types source-importer reader, passed with no override.
  - A copy of `/opt/go` without the go.env line printed `1`.
- **The tester's checks:**
  - With the image ENV, `go env -w CGO_ENABLED=1` kept `0` and printed Go's warning.
  - Under a scrubbed environment, the same `go env -w` setting won over go.env (`1`).
  - A go/build reader under `env -i` ran with cgo on and passed, since gcc was present.
- **Review round 1, finding 2:** the first Dockerfile comment said the ENV prevented a go/build
  reader from failing with `go tool cgo: exit status 2`. That failure had been measured on the
  image without gcc. On the head image the reader passes either way, so the reviewer flagged the
  reason as false. The ENV's real job is to give the reader the same cgo setting as the go
  command.
- **Review round 1, finding 3:** the plan called a dropped go.env line "a known limit". The
  reviewer made it a blocking finding, and the build check closed it. Worker Image run
  40266561462 ran the check.
- **Plan review, round 1:** it rejected a canary run through `go run`, because the exit status
  never came through.

---
title: "A file:// GOPROXY entry pins every version query unless you delete its list files"
category: build-errors
tags:
  - go
  - goproxy
  - module-cache
  - container-images
date: 2026-10-30
status: active
module: packages/daemon/docker
related_issues:
  - "sjawhar/legion#2512"
---

# A file:// GOPROXY entry pins every version query unless you delete its list files

When you ship or configure a warm Go module cache and put it in `GOPROXY` ahead of the public
proxy:

- Delete every `@v/list` and `@latest` file from the cache you ship. Those files name the versions
  the cache holds and no others, a file proxy answers a version query from them **successfully**,
  and a `|` in `GOPROXY` falls through only on an error. So `go get X@latest`, `go get -u`,
  `go list -m -u all`, `go list -m -versions` and a `go mod tidy` that adds an import of a cached
  module all stop at the versions the build baked in, and nothing tells the caller.
- Deleting them costs nothing a pinned build needs: the `.info`, `.mod` and `.zip` of a pinned
  version still come from the cache, and only a version query falls through.
- Refuse the file in the image's own final check, so a later edit of the download step cannot ship
  it again.
- Write `|` rather than `,` where you want a fallback at all. `,` falls through only on a 404 or a
  410, so a proxy outage is not a fallback; `|` falls through on any error. `go.sum` holds each
  module's hash either way, so the second path is a second source, not a weaker check.

## Evidence

In sjawhar/legion#2512 the reviewer reproduced it with Go 1.27.1 against a cache holding only
`golang.org/x/text@v0.3.0`: `go list -m -versions golang.org/x/text` printed `v0.3.0` alone, where
the public proxy answers through `v0.43.0`. The worker image ships
`/gomodcache/cache/download` to every role as its warm cache, so that silence would have reached
every agent that ran `go get` in a pod.

The download step now ends
`find /gomodcache/cache/download \( -path '*/@v/list' -o -name '@latest' \) -delete`
(`packages/daemon/docker/worker.Dockerfile`), with the reasoning in the comment above it and a
refusal in the final stage's toolchain step. The `|` is set in the same stage's
`ENV GOPROXY=https://proxy.golang.org|direct`, whose comment carries the `|` against `,`
distinction.

The same `|` is what makes a Go command survive the public proxy being unreachable, which was one
of the issue's acceptance criteria: `GOPROXY='http://127.0.0.1:1|direct' go mod download` exits 0
through `direct`, and the same command with `,direct` exits 1 with
`dial tcp 127.0.0.1:1: connect: connection refused`.

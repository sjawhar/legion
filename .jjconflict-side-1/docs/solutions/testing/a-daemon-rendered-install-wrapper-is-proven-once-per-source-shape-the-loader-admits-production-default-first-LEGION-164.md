---
title: "A daemon-rendered install wrapper is proven once per source shape the loader admits, production default first"
category: testing
tags:
  - proof-matrix
  - install-wrapper
  - bun-add
  - runtime.kubernetes.plugins
  - worker-pod
date: 2026-10-13
status: active
module: packages/daemon/internal/runtime/sandbox
related_issues:
  - "LEGION-164"
  - "sjawhar/legion#1932"
---

# A daemon-rendered install wrapper is proven once per source shape the loader admits, production default first

Extends docs/solutions/legion/shell-command-gates-derive-from-bash-word-splitting-not-example-forms.md.

- The proof matrix for a shell script the daemon renders is the set of input shapes its config
  loader admits, not the shape you implemented with. Read the loader's validator (here
  `config.readPluginSource`: an exact version, an absolute tarball path, an http(s) URL), render
  the real script once per shape, and run each in the worker pod against the real consumer (`sh`,
  `/usr/local/bin/bun`, the live registry) with a stand-in agent that prints its argv — the
  production default (the released pair's bare versions, linked into the binary) first.
- The renderer classifies a source exactly as the loader canonicalised it. A loader that lowercases
  a URL's scheme and a renderer that tests `https://` case-sensitively disagree on
  `HTTPS://…`; keep one definition of what counts as a path, a URL and a version, and refuse in the
  loader what the consumer cannot fetch (`//host/x.tgz`, `https:///x.tgz`, `http:/host/x.tgz`).
- A negative control is the consumer's own refusal, not the renderer's: the published plugin at
  the wrong contract, installed for real and refused before `exec`.

## Evidence

LEGION-164's install wrapper (`internal/runtime/sandbox/plugins.go` `installScript`) was proven by
its implementer with two `npm pack`ed tarballs only. The tester rendered it for the production
default, `{pi_legion: 9.6.1, pi_envoy: 9.5.1}`, and bun answered
`error: Package "9.6.1" with tag "latest" not found`: a bare version is a package name to
`bun add`, so no pod would ever have installed from the released pair. Fixed by `bunSpec`
(`@sjawhar/<plugin>@<version>` for a version, a path or URL verbatim) with one test per shape
(`TestAVersionSourceIsInstalledAsThePackageAtThatVersion`,
`TestATarballOrURLSourceReachesBunVerbatim`, `TestAnUppercaseSchemeReachesBunVerbatim`), then
re-run in the pod: the version pair installed from the live registry and the agent started at
contract 23, and was refused before `exec` at 24 (`legion plugins: @sjawhar/pi-legion@9.6.1
declares daemon API contract 23; this daemon requires 24`).

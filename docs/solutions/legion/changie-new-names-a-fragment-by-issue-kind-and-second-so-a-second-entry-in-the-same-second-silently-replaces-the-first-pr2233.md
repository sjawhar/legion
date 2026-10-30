---
title: "changie new names a fragment by issue, kind and second, so a second entry in the same second silently replaces the first"
category: legion
tags:
  - changelog
  - changie
  - fragments
date: 2026-10-28
status: active
module: packages/*/.changie.yaml
---

# changie new names a fragment by issue, kind and second, so a second entry in the same second silently replaces the first

- Every package's `.changie.yaml` sets `fragmentFileFormat` to
  `{{.Custom.Issue}}-{{.Kind}}-{{.Time.Format "20060102-150405"}}`, so `changie new` gives two
  entries of one package, one issue and one kind the same file name when it runs twice in the same
  second.
- changie v1.26.0 writes the file with `os.Create` (`cmd/new.go`), which truncates an existing
  file, and exits 0. The earlier entry is lost and nothing reports it.
- For several entries of one package and kind, either write each fragment by hand under a slug of
  its own (`<key>-<slug>.yaml`, the form `AGENTS.md` describes), or run `changie new` at least a
  second apart.
- Then count: the package's new fragments under `.changes/unreleased/` must number the entries
  you meant to add.

## Evidence

sjawhar/legion#2233's merge of main's #2419 (commit 1abb49ba) moved nine changelog entries into
fragments with `changie new --custom Issue=pr2233`, three of them `Fixed` entries of
`packages/envoy`. The script slept just over a second between calls, so they landed as
`pr2233-Fixed-20261028-100849.yaml`, `…-100851.yaml` and `…-100853.yaml`. Without the pause, calls a
few hundred milliseconds apart share a second and leave one file. The review checked the nine
fragments against the removed entries word for word, which is the count that would have caught a
lost one.

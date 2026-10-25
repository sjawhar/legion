---
title: "A claim about what main's image carries, before main's tag exists"
category: testing
tags:
  - worker-image
  - crane
  - evidence
  - github-api
date: 2026-10-25
status: active
module: packages/daemon/docker/worker.Dockerfile
related_issues:
  - "INFRA-372"
  - "sjawhar/legion#2190"
---

# A claim about what main's image carries, before main's tag exists

A change that documents what an earlier pull request put into the worker image is often written
while `main`'s image of that merge is still unpublished (`crane digest
ghcr.io/sjawhar/legion-worker:sha-<12 hex>` answers `MANIFEST_UNKNOWN`). Do not wait for it:

- **Prove the contents on the earlier pull request's head image, and tie that image to `main` by
  the Dockerfile.** List the files with `crane export <ref@digest> - | tar -tvf - <paths>`, read
  the build's own version lines from that head's Worker Image run log, and diff
  `worker.Dockerfile` at that head against `main`.
- **Diff the two files, never GitHub's compare.** `repos/<o>/<r>/compare/<a>...<b>` is a
  three-dot comparison from the merge base: against a squash-merged head it shows the whole
  pull request, not the difference between the two trees. Fetch the file at each ref
  (`gh api 'repos/<o>/<r>/contents/<path>?ref=<sha>' -H 'Accept: application/vnd.github.raw'`)
  and `diff` them.
- **Name it a fallback in the proof and in `Proven by`, and say what it leaves out:** the base
  image and the packages apt resolved at `main`'s build are not the ones proven. Record the
  release run's id and state, so the next role can repeat the listing on `main`'s tag once it
  exists.
- **Build tools under `$TMPDIR`.** `go build <module>@<version>` is refused outside a module;
  `go install github.com/google/go-containerregistry/cmd/crane@v0.20.6` with `GOPATH`, `GOCACHE`
  and `GOMODCACHE` under `$TMPDIR` works, and an image export takes about two minutes.

This differs from
docs/solutions/legion/a-page-that-tells-a-worker-where-to-carry-a-name-ships-the-grep-and-a-skills-change-is-an-image-change-INFRA-221.md,
where the change itself alters the image and only its own final tag can prove it.

## Evidence

INFRA-372 dated a sentence of `docs/kubernetes.md` to the merge of #2149, which added `psql`,
`redis-cli`, `sqlite3`, `nc` and `dig` to the worker image. `sha-6ede52ccb376` and
`sha-158654c89e8b` answered `MANIFEST_UNKNOWN`, and release run 40177490116 was pending. #2149's
head image `sha-98e5f6a8c72c` listed all five and `python3`, its build log printed each version,
and the image before it (`sha-92dbeea3d391`) listed none of the five. GitHub's compare of that head
against `main` returned #2149's whole Dockerfile change. Diffing the two fetched files showed one
comment block (lines 470-472) and nothing else.

---
title: "Keep an attestation's signer identity by calling one reusable workflow build-only, then attest-only, and retag the attested digest with --prefer-index=false"
category: github
tags:
  - github-actions
  - reusable-workflows
  - artifact-attestation
  - cert-identity
  - docker-buildx
  - imagetools
date: 2026-10-28
status: active
module: .github/workflows
related_issues:
  - "INFRA-576"
  - "sjawhar/legion#2358"
---

# Keep an attestation's signer identity by calling one reusable workflow build-only, then attest-only, and retag the attested digest with --prefer-index=false

## The rule

1. When a caller must hold a step between a reusable workflow's build and its attestation (here:
   attest only once the plugins the image links are published), do not move the attest job into
   the caller. The attestation's certificate names the file that ran the attest step, and
   consumers verify `--cert-identity …/<called file>@refs/heads/main`. Call the same reusable
   workflow twice: once with an input that skips its attest job (`skip_attest: true`), once with
   an input carrying the first call's `digest` output that skips its build job
   (`attest_digest`). The certificate still names the called file, at the caller's ref.
2. Write down what the split opens:
   - both calls share the called workflow's concurrency group, so a newer run at the same commit
     can replace the pending attest-only call and leave the digest unattested; the consumers'
     verify then refuses it, which fails closed;
   - any repository can call a public attest-only mode with any digest. That attestation names
     the caller as its source and is stored there, so only `--repo <owner>/<repo>` and the source
     ref in the verify command refuse it. Keep both in the documented command.
3. Add a release's tag to an attested digest with `docker buildx imagetools create
   --prefer-index=false --tag <image>:<tag> <image>@<digest>`, then read the tag back
   (`imagetools inspect --format '{{.Manifest.Digest}}'`) and fail unless it equals the digest.
   Without the flag buildx wraps a single manifest in a new index with another digest, so the
   tag no longer names what was attested.

## Evidence

- `.github/workflows/worker-image.yaml` lines 4-10 and 28-37 (the two inputs), 283-315 (the
  attest job and the cross-repository note), 123-126 (the replaced-call case);
  `.github/workflows/release.yaml` lines 993-1039 (the two calls).
- Branch dispatches of `release.yaml` on sjawhar/legion#2358, runs 40451569882 and 40538213611:
  the attest-only call attested the build-only call's digest, and the certificate's SAN was
  `https://github.com/sjawhar/legion/.github/workflows/worker-image.yaml@refs/heads/legion/INFRA-576`.
- `release.yaml` lines 1078-1093 (`worker_image_release`). Probe run 40451765371 on the runner's
  buildx v0.38.0: with `--prefer-index=false` the tag named the source digest
  `sha256:8354eb26…`; without it, `sha256:de61b6c7…`.
- The rule that the calling job grants every scope the called file's `permissions:` blocks
  declare, even for a job the call skips, is
  `docs/solutions/github/pull-request-trigger-paths-follow-the-pr-head.md`; it holds for both calls.

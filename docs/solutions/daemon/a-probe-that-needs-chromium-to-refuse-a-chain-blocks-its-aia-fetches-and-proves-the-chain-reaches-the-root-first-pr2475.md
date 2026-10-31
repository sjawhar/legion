---
title: "A probe that needs Chromium to refuse a chain blocks its AIA fetches, and proves first that the chain reaches the root"
category: daemon
tags:
  - tls
  - chromium
  - aia
  - probe-image
  - worker-image
date: 2026-10-28
status: active
module: packages/daemon/cmd/legion/tls_probe.go
---

# A probe that needs Chromium to refuse a chain blocks its AIA fetches, and proves first that the chain reaches the root

- Chromium's certificate verifier fetches the AIA "CA Issuers" URLs of a chain it cannot anchor,
  and it gives its verdict only after those fetches end. A CA's repository host that stalls holds a
  refusal for 10 s or more. The log line is
  `cert_issuer_source_aia.cc:138 AiaRequest::OnFetchCompleted got error -7` (`ERR_TIMED_OUT`).
  curl, OpenSSL, Python, Go, Node and Bun fetch no AIA, so only Chromium waits.
- Chromium has no switch that turns AIA fetching off; only the standalone `cert_verify_tool` has
  `--no-net-fetcher`. `--host-resolver-rules=MAP * ^NOTFOUND, EXCLUDE <probed host>` gives the
  same result for one process: each fetch fails at once with `ERR_NAME_NOT_RESOLVED` (-105).
  `^NOTFOUND` is the token net/dns/mapped_host_resolver.h documents. `~NOTFOUND`, which Chromium's
  SOCKS proxy page shows, is read as a replacement host name instead; it fails only because the
  resolver rejects `~notfound` as a malformed name. The rule also leaves an environment proxy's
  host unresolvable, so behind a proxy every load fails.
- Without AIA, Chromium's verdict comes from the served chain and its local trust store. That is
  the verdict to prove only if the site serves every certificate up to the distrusted root's
  subject. The probe's positive control proves this on each run: curl with only the pinned root
  must accept the site. A refusal could otherwise come from a chain Chromium could not complete.
- A path that only an AIA fetch completes is now invisible to the probe, while a worker's Chromium
  still fetches AIA. Check what the AIA URLs serve. A self-signed root there adds nothing, because
  a path can end at it only as a local anchor. A cross-certificate from a trusted root there would
  be a false pass.
- To reproduce the stall without waiting for the CA's host, resolve the AIA host to an address
  that drops packets (`10.255.255.1`) in the image's `/etc/hosts`.

## Evidence

PR #2475. With `repository.emsign.com` blackholed, the image's own `legion probe-image` failed with
the CI text `chromium cannot verify refusal of the emSign chain: signal: killed` after 40 s. The
patched probe passed in 20 s, and passed 10 of 10 runs under load. When the emSign roots or the
G3B intermediate were trusted in `~/.pki/nssdb`, Chrome for Testing 158 with the rule reported
`ERR_CERT_REVOKED`. With Debian's chromium 156, which trusts emSign, the probe failed with
`chromium accepts the emSign chain`.

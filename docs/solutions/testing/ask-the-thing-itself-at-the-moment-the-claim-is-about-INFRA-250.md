---
title: "Ask the thing itself, at the moment the claim is about"
category: testing
tags:
  - evidence
  - negative-control
  - ci
  - test-isolation
  - nats
date: 2026-10-25
status: active
module: packages/envoy
applies_when:
  - You are about to assert on a handle, a cached value or a local the test already holds
  - You are about to quote CI, a log or a job while it is still running
  - You are adding a pre-flight check before an operation that would fail anyway
related_issues:
  - "INFRA-250"
  - "sjawhar/legion#2189"
---

# Ask the thing itself, at the moment the claim is about

## When a change explains a mechanism twice, the two explanations are each other's control

This one comes first because nothing in a repository performs it. A lane runs code, a reviewer
reads a diff, a sweep matches a pattern — **none of them compares two sentences about the same
mechanism.** Two explanations can each survive being read against the code and still contradict
each other, and the contradiction is invisible until someone holds them side by side.

**So: whoever edits one explanation of a mechanism reads every other explanation of that mechanism
in the same change, in the same sitting.** It costs minutes, it is nobody's job, and it is the only
check there is.

The rest of this note is one rule in four shapes. A check is only evidence when **its object is the
claim's object**, observed **when the claim is about**:

- **Read the system's state, not the handle you hold.** An assertion that a bucket still exists
  queries the server's stream list; a handle the test opened earlier proves only what the test
  believed at open time.
- **Quote the verdict you can actually read, and say which one it is.** A lane's conclusion and a
  line of its log are different claims. GitHub serves a job's log only once the whole run finishes,
  so until then the honest sentence is "the lane is green", not a paraphrase of output you have not
  seen. Upgrade the claim when the evidence arrives; never pre-date it.
- **Let an operation be its own check.** A pre-flight probe tests the world at a moment; the
  operation tests it at the instant that matters. Where the operation already fails loudly and names
  the cause, a separate probe adds a race and a second thing to keep true.
- **A filter is a claim about a name, so prove the name.** `go test -run <pattern>` reports `ok`
  when the pattern matches **nothing**, so a renamed test's own green run cannot tell you the
  rename took. Run the old name too and require **zero** tests, or read the `=== RUN` lines.

The corollary is a rule about silence: **a check whose object differs from the claim's is worse than
no check**, because it reads as evidence. A readiness probe of a fixed TCP port answers "something
is listening", while the claim is "the server I started is ready" — the same sentence shape as a
guard that matches an error code the failure never returns.

## When a green run is not a control

Name the configuration the defect lives in. If the failure is rare by construction — a race, a
window, a timing gap — a passing run proves nothing about it, and offering one as proof is a
category error. Prove the **mechanism** instead: show the precondition that makes the window
impossible, and make that precondition itself assertable so it cannot silently decay.

For the same reason, name the configuration you **cannot** run. A Legion worker pod has no C
compiler, so `go test -race` cannot run there at all; in-pod runs of the non-race configuration are
runs of a different configuration and are not evidence about the race. Say so where the proof is
recorded, and let the lane that can run it be the one that answers.

## When you hold two copies of something, compare them before you trust either

The same rule applied to your own records. **Two recovery paths that disagree are one path plus a
trap**: a file banked in version control and a copy on disk drift the moment either is edited, and
the instinct under pressure is to trust the copy you made yourself. Compare them by hash and
refresh from the authoritative one, in that order.

## Evidence

- A subtest pair shared one JetStream account because the test took its URL in the parent, so one
  subtest deleted the account's last stream while the other recreated the name; the fix gives each
  subtest its own account, and the proof quoted is the lane that runs `-race`
  (`ok github.com/sjawhar/envoy/internal/bus 207.027s`, 0 `DATA RACE`), not a pod run of the
  configuration that never failed.
- The guard that keeps one test's account non-empty is asserted by walking `js.StreamNames()`
  immediately before the delete, not by trusting the handle returned when the keeper was created:
  removing the keeper then fails the test by name instead of silently reopening the window.
- `scripts/e2e-local.sh` was reported PASS against a NATS server it had not started, because
  readiness was a TCP probe of a fixed port: the port answered, whoever opened it. Readiness now
  requires the script's **own** child — `kill -0` on its pid, then its own log's ready line, then
  the greeting. The port is not pre-checked: the bind is the check, it cannot be raced, and its
  failure names the port in the server's own words.
- A `StartGate` that stopped reading `/proc/sys/net/ipv4/ip_local_port_range` would have passed
  every committed test. The test now reads that file itself and checks 20 gate ports against it —
  with no injection seam, because a seam is a different object from the behaviour. Its limit is
  recorded: on a machine whose ephemeral range begins at the rig's fallback, the mutation passes,
  and only a runner with a different range catches it.
- A guard test whose comment and **name** asserted a universal the code did not check was renamed
  along with its comment. The rename was proven by running the **old** name and requiring zero
  tests: `go test -run TestNoPackageOutsideTestnatsNeedsAContainerRuntime` prints `ok` for a test
  that no longer exists, so the new name's own pass proves nothing about the rename.
- **The twin explanations.** This change explained one nats-server mechanism in two comments. The
  keeper comment had it right; `stream_config_test.go`'s named the wrong removal — the deleted
  stream's own directory, which nats-server renames aside and removes as a renamed copy
  (`filestore.go:8757-8781`), rather than the account's streams directory, whose `os.Remove` every
  delete schedules and which is a no-op while anything remains (`stream.go:5288-5293`). Both
  sentences had been written by reading the code; neither had been read against the other. It
  survived a tester round and a reviewer round, and the tester found it by reading nats-server
  rather than either sentence. The second of the two was itself written to correct a third.
- **The two copies.** These notes were banked in a jj change while another role held the working
  copy, with a copy kept outside the workspace. The outside copy was the stale one — it predated a
  rule added later — and was refreshed from the banked change rather than the other way round,
  after comparing both by SHA-256.

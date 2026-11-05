---
title: "A token fake that answers every owner, and a fixture whose two answers agree, make an owner assertion vacuous"
category: testing
tags:
  - credentials
  - test-doubles
  - negative-control
  - daemon-go
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2648"
---

# A token fake that answers every owner, and a fixture whose two answers agree, make an owner assertion vacuous

Extends docs/solutions/testing/prove-which-credential-a-caller-presents-without-minting-or-printing-one-INFRA-228.md,
which is about proving a live caller's identity. This one is about the unit test's fake.

When the thing under test is **which principal a call was made as** — which owner a token was
minted for, which account a client addressed, which tenant a row was read in — the test proves it
only when both of these hold:

- **The fake refuses every principal but the right one**, as the real service does. A fake that
  answers `token-for-<whatever-you-asked>` cannot fail, whatever the code does, so the assertion
  on its output passes for the wrong principal too.
- **The fixture is one where the right and the wrong answer differ.** Check this by reading the
  mapping's own rules, not by reading the fixture's names: a pair that looks like a rename may be
  no rename at all under the rule being tested.

Then prove the test can fail: revert the one line, run it, and quote the failure in the handoff
and on the thread. A test whose revert still passes is a test that measured nothing.

## Evidence

In sjawhar/legion#2648, `TestTheBackfillOfAConfiguredRenameFillsTheRecordsOwnName` was written in
round 2 as the proof of round 1's finding that a configured repository rename must not turn an
unfilled record into a boot-time refusal. The reviewer accepted it in round 2. In round 3 the
reviewer found the defect it was named for still live, and the test still green. It failed in two
independent ways at once:

- Its token source was the package's `roleTokens{}`, which answers `role-app-token-for-<owner>`
  for **any** owner it is asked for. The reviewer: "the test passes only because `roleTokens{}`
  ignores the owner".
- Its fixture was `acme/old → acme/new`, a rename **inside one owner**. The rule under test,
  `ghrepo.Renames.OwnerAliases`, makes such a pair no alias at all, so the mapping was the
  identity function on that fixture and nothing could ever have failed, whatever the token source
  did.

The repair is the idiom worth copying. `servedOwnerTokens` in
`packages/daemon/internal/daemon/backfill_spelling_test.go` refuses every owner but the served
one, exactly as GitHub refuses an installation that no longer covers a repository; the fixture
became a cross-owner pair; and the control is the revert, which answers

```
fill the repository of LEGION-1: mint the implement App token to read the default branch of
sjawhar/legion: no installation of the implement App on sjawhar covers the repository
```

`review_permission_test.go` reuses the same fake for the review paths, and its no-pair subtests
expect that refusal — which is itself the proof that the fake checks what it is asked for.

`roleTokens{}` is still the token source of about twenty-five tests in `internal/daemon`. It is
right for every test that does not assert an owner, and a trap for the next one that does.

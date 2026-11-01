---
title: "Evidence can be absent rather than wrong, and absence reads exactly like success: for every check you cite, name what would have made it fail"
category: testing
tags:
  - evidence
  - review
  - proofs
  - negative-control
  - citations
date: 2026-10-21
status: active
related_issues:
  - "INFRA-31"
  - "sjawhar/legion#2085"
---

# Evidence can be absent rather than wrong, and absence reads exactly like success: for every check you cite, name what would have made it fail

A wrong result argues with you. An absent one agrees with you: the command exits 0, the citation
resolves, the suite is green, the control runs. Every face below was found on one issue, by
different roles, and in each the artefact looked exactly like the one that would have proved the
claim.

- **A citation that resolves but does not support its sentence.** The reviewer followed a `path:line`
  in a handoff, found a real symbol there, and only on reading the function noticed it established
  something adjacent to the sentence citing it. Resolving is not supporting; the check is to read
  the cited lines *as the answer to the sentence's question*.
- **A command that exits 0 having resolved an impostor.** `bunx <tool> --version` prints a version
  for a tool the project does not install, because `bunx` fetches it. The discriminator that
  separates them costs one line: run `node_modules/.bin/<tool> --version` and watch it fail
  (INFRA-71).
- **An assertion derived from its subject.** A substring pin over rendered output stayed green with
  the derived constant set to zero, because the expectation was built from the same expression the
  code under test used. A test whose `want` is computed by the code it tests cannot fail; write the
  number by hand, per case, and the test becomes an argument (`plugins_test.go`'s wait-bound table).
- **A control that cannot interleave.** A concurrency test's "without the lock" arm proved nothing
  while its two installs could not overlap in time; the control has to be shown *failing* —
  removing the fence has to turn the suite red — or it is decoration.
- **A fence whose removal leaves the suite green.** Delete the `flock` call and re-run: if nothing
  goes red, the fence is untested whatever its coverage says. This is the same move as the control,
  applied to production code instead of the harness.
- **A check derived from a claim's shape, which cannot see a false clause inside it.** An
  enumeration that asks "is every rendered path accounted for in this paragraph" passes whether
  the reasons attached to those paths are true or invented. Structure and grounds are two checks,
  and only the first can be automated (tester).
- **A lane name taken as a claim about a run.** A green name beside a pull request says a *job* was
  green, not that the thing you changed ran. Read the run (see
  docs/solutions/github/a-green-lane-name-is-not-a-run-read-the-run-and-page-the-check-list-INFRA-31.md).

The transferable rule is one sentence, and it is cheap enough to apply to every claim you make:
**for any check you cite, name what would have made it fail.** If you cannot name it, you have
recorded an absence.

## Two guards on applying it

- **Carry the lead, re-derive the clause.** The failure mode of this rule is "distrust findings",
  which costs more than it saves — most findings here were right, and re-deriving everything is how
  a twelve-round review happens. Keep what another role pointed you at; re-derive the clause you
  are about to repeat in your own words, starting with **the sentence that makes your argument
  strongest**, because that is the one you are least inclined to check. A reviewer's imported
  `20Gi` and an architect's invented cause both entered this issue that way: each was a strong
  sentence adopted from a neighbour without the one read that would have broken it.
- **Write the decision rule before the result arrives.** Pre-commitment here was not a virtue, it
  was timing: the rules that held — "report the fingerprint as changed if it changes", "the mutation
  must turn the suite red" — were written down while the outcome was still unknown, when stating
  them honestly cost nothing. An hour later both would have been easy to shade. So when you notice
  you are about to learn something that could embarrass a position you hold, write the rule first.
  Sequencing, not courage.

## A claim about production is the same shape

No role in a Legion tree can read the cluster. A sentence about production in operator-facing
documentation therefore needs the same fence as a derived constant: **it names what it rests on**,
so the next reader can tell a measurement from an inference without re-running the night. The
contrast from this issue's own history: "2.1 to 2.7 GiB, measured on live volumes at 01:45Z" names
its basis and survived review; "the duplication is being removed" named nothing and was false.

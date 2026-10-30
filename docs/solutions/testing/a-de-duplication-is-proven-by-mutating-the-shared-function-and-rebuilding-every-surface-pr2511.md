---
title: "A de-duplication is proven by mutating the shared function and rebuilding every surface"
category: testing
tags:
  - negative-control
  - contracts
  - refactor
  - changelog
date: 2026-10-29
status: active
module: packages/contracts
---

# A de-duplication is proven by mutating the shared function and rebuilding every surface

Two surfaces that must read one answer the same way are not proven to by an unchanged test file,
by a grep, or by an empty diff. Those show that one surface still passes its own assertions; a
hand copy on the other side leaves it green.

- **Mutate the shared function to return a sentinel, rebuild every artifact that embeds it, and
  watch every surface move.** For the dashboard that means rebuilding the SPA bundle the browser
  serves, not only running the unit suite. Revert, rebuild, and watch them move back. A surface
  that did not move still holds a copy.
- **Keep the behaviour-preserving guard too.** A suite that already pins the old surface's output
  byte for byte, run unmodified, is what says the move changed no wording; the mutation is what
  says the other surface reads the same function. Neither replaces the other.
- **When you find yourself writing "package A cannot import package B, so this mirrors B's
  sentences", you have found a shared module, not a licence to duplicate.** Move the judgement up
  into the package both already depend on. When the shared thing is a classifier rather than a
  literal, share the classification as a discriminated union and let each caller word it, and
  write the branch order into the doc comment as the contract: reordering it then changes both
  surfaces at once.
- **Add the new module's row to its package's `AGENTS.md` "Where to look" table in the same
  change.** That table is how the convention is found; a module missing from it is a module the
  next author will duplicate instead.
- **"No output changes" is a claim about the inputs the producer can send.** When the move drops a
  defensive coalesce because the contract types the field as required, scope the claim to the
  answers a server actually sends, cite where the server constructs that field, and name the input
  whose behaviour did change. A differential sweep over generated inputs is what makes the scope
  precise rather than hopeful.

## Evidence

sjawhar/legion#2511. The plan decided that the dashboard would mirror the CLI's three sentences
because the web app cannot import `@legion/envoy-client`; that decision passed plan review and was
overturned in code review, because both packages already depend on `@legion/contracts`. The
classifier moved to `packages/contracts/src/related-forms.ts`, and the CLI's lines and the
dashboard's section were each reduced to wording its union.

- The behaviour-preserving guard: `packages/envoy-client/src/__tests__/dispatch-advice.test.ts`,
  57 assertions over the printed advice, run unmodified — 57 pass, with the test directory and the
  contract types untouched by the change.
- The implementer's first control was that empty diff, which proves only the CLI's half. The
  tester replaced it: `relatedCheckModeWords` changed to return `MUTATED-MODE-WORDS` and the SPA
  rebuilt, after which the CLI printed `Related items (searched by MUTATED-MODE-WORDS: …)` and the
  dashboard's section read the same words; reverted and rebuilt, both read `keywords only` again.
- The scope of the claim: the move dropped `suggestions.related ?? []` because the contract types
  `related` as required and the server constructs it even for a check that was not made
  (`packages/envoy/internal/dispatch/api/suggestions.go:177,376`). A differential sweep over tens
  of thousands of generated answers found no difference wherever `related` is an array, and a
  difference only for one that is absent or null. The changelog fragment first read "No line a
  reader sees changes" and was corrected in review to scope it to the answers a server sends and
  to name that one shape.

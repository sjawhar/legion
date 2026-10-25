---
title: "A ruling posted after the plan is acceptance: re-read the issue before each push and carry it to every copy"
category: legion
tags:
  - implementer
  - scope
  - acceptance
  - agents-md
  - find-every-copy
date: 2026-10-25
status: active
module: skills/legion-worker
related_issues:
  - "INFRA-372"
  - "sjawhar/legion#2190"
---

# A ruling posted after the plan is acceptance: re-read the issue before each push and carry it to every copy

`plan.json` is frozen when the plan is approved. An architect who widens the issue afterwards does
it in a comment or an Envoy message, and nothing copies that into the plan's acceptance list, the
tester's checklist or the throwaway acceptance check. So:

- **Re-read the issue's comments and events before every push and before you complete,** not only
  at the turn's start. A ruling that arrives mid-turn is not in the context you started with. The
  plan names the work you were given; the issue names the work that is due.
- **Turn each widened item into acceptance before you write its code:** a `proof` entry with its
  own criterion, command and negative control, and an extension of any acceptance script that
  would otherwise refuse the new edit. An item that is not in the acceptance list is not proven,
  and the tester follows the same list, so the reviewer is the first to notice.
- **Carry a changed command form to every copy, the auto-loaded `AGENTS.md` copy first.** Search
  for the command's stem (`auth can-i`), not for each full command: prose lists abbreviate
  (`` `kubectl auth can-i create jobs`, `create services` and `get services` ``), so a grep for
  the second or third command misses that copy. `AGENTS.md` is loaded into every session, so a
  role copies the command from there before it opens the page. The general search rule is
  docs/solutions/legion/find-every-copy-of-a-hand-maintained-list-by-its-last-member-alone-not-by-a-phrase-LEGION-764.md;
  the delta here is the stem and the order.

## Evidence

INFRA-372's plan was approved for two clauses. At 09:02Z the architect ruled a third item in by a
comment on the issue (add `-n <namespace>` to the section's three gate commands). The
implementer's turn had read the issue's events before that comment existed. Round 1 completed
without the item, and the tester's round-1 proof rows covered only the plan's criteria. The
reviewer requested it: one round. Round 2 changed the page's gate block and left
`AGENTS.md:128`, which names the same gate in the abbreviated form, without `-n`. The reviewer
requested it again: a second round. A stem grep (`auth can-i`) at the start of round 2 would have
listed both copies.

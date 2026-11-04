---
name: following-best-practices
description: "Use before choosing a design, writing options, a spec or plan, or requesting approval."
---

# Following best practices

A design a person sends back usually skipped practices that everyone who builds such things knows to follow, or stopped short of a world-class answer for no reason the case required. Its author knew both when asked, but nobody asked until the person did, after the work was done.

- **Best practice** is every practice that people who build this kind of thing know they should follow, and often skip. It is the floor.
- **World-class** is the best answer for this problem, with time and money no object. It is not over-engineering, and it is not built for a scale the work does not have. It runs its own infrastructure when that is the better answer, takes on complexity where the problem earns it, and takes on hard engineering gladly. It is the target.

## 1. Before you design

Before you write a design, a recommendation or options, start a fresh subagent and send it only the problem: what must work, for whom, the constraints and numbers, the person's words that describe the problem, and the setup. Leave out every design, option or leaning you have, so a design you already have in mind does not choose the answers.

```text
<The problem only.>

From this problem alone, research and answer three questions. Do not ask for or propose a design.
1. What is best practice here? Every practice that applies to this kind of problem.
2. What is the world-class answer? The criteria that make it the best answer for this problem, and each action they ask for (a drill or a test included).
3. Which fact of this setup, if any, rules out a practice or the world-class answer? A cost in time, money or effort is not such a fact.

Answer as one numbered list, 1 to N, each number once at the very start of its line (`1. ` or `**1.** `, no indent, no heading): every practice, every action a world-class criterion asks for and every practice a fact rules out is its own item. Put nothing of substance outside the items, and number no other lines. Each item gives the practice or action, a source you read (the tools' own documentation, two or three well-regarded projects) as a link, and why it applies to this case: the facts of this problem that make it matter here; "it is a best practice" is not a reason. An item a fact rules out also names that fact.
```

Wait for its answers in the same turn: in Claude Code, start it with `run_in_background: false`; in omp, `wait` for its task. Never end your turn while it runs: in a headless session, the end of your turn stops every subagent still running, and its answers never arrive.

Then, before any action that states or leans to a design (text you write, a file, a subagent brief, or a command's arguments, such as an issue title, even in a dry run), save its whole answer, unchanged, to `research.md`, and write the practice table to `table.md`: one row per item number, `| # | Practice | Source | Why it applies to this case |`, filled from that item. For an item with no link or with no reason that names a fact of this problem, ask a research subagent for the missing part (in Claude Code, a fresh subagent given its answer, started with `run_in_background: false`), add its answer to `research.md`, and fill the row from it, never from your own words. Then run this check, and fix and run it again until it prints `OK`:

```bash
python3 - research.md table.md <<'EOF'
import re, sys
research, table = (open(p).read() for p in sys.argv[1:3])
bad, items, rows = [], [], {}
for i, line in enumerate(research.splitlines(), 1):
    m = re.match(r"(?:\*\*)?(\d+)[.)](?:\*\*)?\s+\S", line)
    if m:
        items.append(int(m.group(1)))
    elif re.match(r"[\s>#*_+-]*\d+[*_]*[.)][*_]*(\s|$)", line) or line.lstrip().startswith("|"):
        bad.append(f"research.md line {i} is numbered or a table but not a top-level item: {line.strip()[:60]}")
if not items or items != list(range(1, len(items) + 1)):
    bad.append(f"research.md numbers its items {items}, not once each from 1 to N")
for line in table.splitlines():
    cells = [c.strip() for c in re.split(r"(?<!\\)\|", line.strip().strip("|"))]
    if line.lstrip().startswith("|") and cells[0].isdigit():
        rows.setdefault(int(cells[0]), []).append(cells)
for n in sorted(set(items) | set(rows)):
    r = rows.get(n, [])
    if n not in items:
        bad.append(f"row {n}: no such item in research.md")
    elif len(r) != 1:
        bad.append(f"item {n}: {len(r)} rows, not 1")
    elif len(r[0]) != 4:
        bad.append(f"item {n}: {len(r[0])} cells, not 4 (# | Practice | Source | Why)")
    elif not all(c.strip("-–— ") for c in r[0][1:]):
        bad.append(f"item {n}: empty practice, source or reason")
print("\n".join(bad) or f"OK: {len(items)} items, {len(items)} rows")
sys.exit(1 if bad else 0)
EOF
```

Take no action that states or leans to a design before it prints `OK`. It checks only that every item has one complete row; that each source is real and each reason fits this case is yours to check while you fill the rows. Recording them does not end your turn: go on with the work.

Then:

- Follow every best practice unless a fact of this case rules it out, and write that fact in the design. Less work, less money, no new credential, or a precedent here that does it another way is not such a fact.
- Design the world-class answer. Fall short of it only when a fact of this case requires it, and name each shortfall with its reason. Name a cost the person must accept as a cost, for them to decide.
- Settle a disputed fact with evidence: the documentation, the code or a probe. Ask the person only for a cost to accept or a choice of priority.
- In a spec, open the `Design` part with a `### Best practice and world-class` entry copied from step 1's table: each best practice with its source, why it applies to this case and whether the design follows it, the world-class answer, and each place the design falls short with its reason. The approver then sees the comparison without asking for it.

## 2. Before each approval request

Start two fresh subagents, one per brief, and wait for each answer in the same turn, as in step 1. A fresh subagent starts with no history and gets only the text you send it. In each first-round reviewer's first message, give only the problem: what must work, for whom, the constraints and numbers, the person's words that describe the problem, and the setup. Do not include the proposal's title, design, options, decisions, recommendation or conclusion, or a person's words that state a proposal: a reviewer that reads the design first tends to accept it. After it answers, paste the whole proposal's text into your next message to that same subagent and ask it to compare. A path, a file name, a summary, or a note in place of a table or section is not the proposal, even when the reviewer can read files: paste every part as you wrote it. If your harness cannot message a finished subagent and wait for its answer, give a second fresh subagent the first reviewer's answer and the whole proposal's text, and ask it only to make that comparison; in Claude Code, a message to a finished subagent resumes it in the background, so start that second subagent with `run_in_background: false` instead. Where your harness offers a subagent on a different model family, use it for these reviews, because a reviewer on the author's model shares its blind spots.

Best practice:

```text
First message:
<The problem only.>

From this problem alone, list every best practice for this kind of problem: each practice that people who build such things know they should follow and often skip, with a source you read. Do not inspect or ask for a design.

Follow-up:
<The whole proposal's text, pasted.>

List each best practice the proposal does not follow, and whether it states a fact of this case that rules the practice out. Skipping a practice to save work, time, money or a credential is a finding. End with FOLLOWS BEST PRACTICE, or the findings.
```

World-class:

```text
First message:
<The problem only.>

Assume time and money are no object. World-class is not over-engineering and is not built for a scale the work does not have; it runs its own infrastructure when that is the better answer, takes on complexity where the problem earns it, and takes on hard engineering gladly. From this problem alone, write what a world-class solution would look like. Do not inspect or ask for a design.

Follow-up:
<The whole proposal's text, pasted.>

List each place it accepts less than that answer (a manual step, a workaround, a second system, a narrower scope, a temporary fix), and whether the proposal gives a fact of this case that requires it. End with WORLD-CLASS, or the findings.
```

- Paste the proposal by code that reads its file into the message, never by retyping or summarizing it. Before you post anything about the proposal to a person (an ask, a comment, a message or the approval request), start one more fresh subagent and wait for it in the same turn (in Claude Code, with `run_in_background: false`): send it the proposal file's text as it was at each send, each follow-up message you sent the reviewers, and both reviewers' final replies with the times they arrived. Ask it whether each follow-up holds the whole file text, the practice table included, and whether both final replies arrived. Post nothing until it answers that both hold; send the whole text again and wait for the new verdicts for each thing it names.
- Fix each finding from either review, or record it in the `### Best practice and world-class` entry with the fact or the cost that requires it. Never request approval while a finding is neither. A NOT WORLD-CLASS verdict whose findings are all fixed or recorded does not block the request.
- Write the final round's verdicts of both reviewers in that entry, not in another section: for each reviewer, its own verdict, its model (such as `anthropic/claude-opus-5`, never its role or agent name), the id of its run (the agent id or session id your harness gave it; the two reviewers are two runs with two ids) and the clock time (UTC) its final answer arrived. Take that time from your harness at the moment the answer arrives: run `date -u` as your next action, write the time and the run id to a file at once, and copy the entry's times from that file. Never write the time later from your own clock, from the time you edit the entry, or from text the reviewer or you wrote. Name the verdicts in the approval request, so the approver sees that the reviews ran and what they found.
- Run step 2 again before a later approval request when the design, the problem, its constraints or the evidence changed since the last review.
- Stop after three rounds of step 2 on one proposal: list each change made after the last round and each finding still open, with its cost, in the entry. Review loops between agents do not end by themselves.
- When a reviewer names a practice or an answer you did not consider, go back to step 1 with it instead of defending the design.
- When a person questions the approach, in any words, run step 1 again before you answer.

---
title: "Scrubbing tmux's environment tables: parse `show-environment -s` as one text, never probe a name through tmux's argv, and verify the table afterwards"
category: daemon
tags:
  - tmux
  - show-environment
  - set-environment
  - argv-retokenisation
  - show-messages
  - locale
  - parser
  - invariants
  - names-not-values
date: 2026-09-13
status: active
module: packages/daemon
related_issues:
  - "LEGION-74"
  - "sjawhar/legion#1007"
symptoms:
  - "the boot log's `removed N variable(s) …` line contains a base64 fragment or the word `export`"
  - "an allow-listed variable (HOME) is missing from the tmux server's global table after a boot"
  - "`tmux show-messages` on the private server lists `unknown variable: <fragment>` lines"
  - "a boot refuses with `tmux show-environment … failed (exit 1): unknown variable: …`"
---

# Scrub tmux's Environment Tables by Parsing `show-environment -s`, Never by Probing Names

## The problem

At boot the daemon must remove from the private tmux server's global table, and from its
session's table, every variable that is not a key of the allow-listed `paneEnv`
(`spawned-process-environment-is-an-allow-list-and-the-tmux-tables-are-part-of-it.md`). That
needs the *exact* set of names in each table, and the whole operation must log and throw with
names only — a value in that table is, by hypothesis, a secret. Three designs went through four
review rounds; the first two were wrong in ways worth remembering, because both looked correct on
every table anyone had seen.

## Design 1: the bare dump, one line per name (wrong)

`show-environment -g` prints `NAME=value` per entry and `-NAME` per unset marker; the parser took
everything up to a line's first `=` as a name. A value's own newlines are printed literally when
the client's locale is UTF-8, so a multi-line value's continuation lines look like entries: the
`=`-padded last base64 line of a PEM (`ZmFrZS1rZXktYnl0ZXMtbm90LXJlYWw=`) became a "name", was
`-u`'d as a harmless no-op, and — the real failure — **appeared in the boot log**, which had
promised names only. Bash function bodies were not the trigger (bash indents their lines); a raw
multi-line secret exported into the launcher's environment was.

## Design 2: confirm each candidate with a per-name probe (wrong)

`show-environment <table> <name>` exits 0 when the name is an entry and 1 with `unknown variable:
<name>` when not, so confirm every candidate before unsetting or logging it, reading only the exit
code and stderr. This fixed the PEM case and failed on two shapes nobody had produced yet, both
because **tmux re-tokenises its argv**:

- **A trailing `;` is tmux's command separator and is stripped from the token.** A continuation
  line `HOME;=x` yields the candidate `HOME;`, which probes as `HOME` → present → confirmed →
  `set-environment -u 'HOME;'` runs as `-u HOME` and **removes the real, allow-listed `HOME`**
  from the global table. Every pane opened afterwards, across restarts, lacks `HOME` until the
  server is recreated. The log records `HOME;`. Nothing in the daemon could notice.
- **Trailing whitespace** (`key = value` → candidate `key `) survives argv but tmux echoes
  `unknown variable: key ` with the space, so an exact stderr comparison that trimmed one side
  threw — a **boot refusal whose message named a value fragment**.

And a residue the deep audit found: tmux records each command's argv and its reply in the
server's own `show-messages` log, same-uid readable like `/proc/<pid>/environ`. Sixty-six probes
per boot on production each left `unknown variable: <fragment>` there. Two tmux calls per
candidate, too.

The general lesson: **anything you hand to tmux as an argv token is re-tokenised**. A trailing
`;` becomes a command separator (`\;` is the literal), a leading `-` is a flag. Never let a
string you parsed out of tmux's *output* become a name you pass back into tmux's *input* unless
you have proved it round-trips, and never let tmux's reply text — which quotes the token —
reach a log or an error.

## Design 3: parse `show-environment -s` as one text (landed)

`show-environment -s` prints shell syntax: each set entry is `NAME="value"; export NAME;`
followed by a newline, with `$`, `` ` ``, `"` and `\` escaped by a backslash inside the value and
the value's own newlines left in place; each unset marker is `unset NAME;`. `parseShellEnvironment`
(`tmux.ts`) scans the whole dump, never line by line:

- A name is everything up to the first `=` (tmux refuses `=` in a name; a space, `"` or `;` in
  one is fine — `A B`, `Q"N`, `X;` all print and parse).
- The closing `"` is the first *unescaped* one, and an entry closes only on the exact
  `"; export <the same NAME>;` + newline. A continuation line — whatever it contains, even the
  text `"; export X;` itself, which arrives escaped — can therefore never read as a name.
- Marker or entry is decided by **structure**, not by the `unset ` prefix alone: a line is a
  marker only when it is exactly `unset <NAME>;` followed by a newline and contains no `=`. So an
  entry literally named `unset X` (settable via `set-environment`, not by a shell) parses as the
  entry it is, and a marker missing its `;` throws instead of swallowing the next entry (the
  round-4 residual — the first version tested the prefix first).
- Anything else is a parse failure naming the table and the **byte offset**, never the text
  there. Zero per-name tmux calls: one listing per table, one `-u` per removed name, one
  re-listing.

The unset side escapes exactly one thing: a name ending in `;` is passed as `\;`, which tmux
restores to the literal (verified on 3.7c: `-u 'X\;'` removes `X;` and leaves `X`). A name
beginning `-` is refused by tmux's flag parser — a loud names-only boot refusal, accepted as
fail-closed.

### The two post-scrub invariants

After the `-u`s, each table is listed again and checked; a violation throws, names only:

1. every allow-listed name that was present is still present;
2. no removed name remains.

They are sufficient because every parsed name is in exactly one of `kept`/`dropped`, so a `-u`
that lands on the wrong entry either removes a kept name (1) or leaves a dropped name (2). This
is the guard against any argv surprise the escape rule missed — it would have caught Design 2's
`HOME` removal on the spot instead of leaving panes without `HOME` for a server lifetime. Cost:
one extra listing per table, only when something was dropped.

## Locale changes the dump, not the parser

tmux renders a value's embedded newlines literally only when the **client's** locale is UTF-8.
In the C locale it vis-encodes every non-printable byte as `_` — one line per entry, and also
`_` for non-ASCII bytes in *names*, which yields a false name whose `-u` is a no-op and which
invariant 2 then reports. The daemon's `paneEnv` carries the operator's `LANG`, so which
rendering the daemon sees depends on the deployment; the `-s` parser handles both identically,
and the tests carry both renderings of the same PEM.

## The smaller mistakes the rounds corrected

Each of these looked fine until a reviewer asked "what does the code do on an input the author
never produced?" — the same question that killed Designs 1 and 2.

- **Read-then-disable is a race.** Reading the session table *before* emptying its
  `update-environment` lets an operator attach landing in between copy a fresh `SSH_AUTH_SOCK`
  behind the read. Disable first, then read; `disableEnvironmentUpdates` also tells the caller
  whether the session exists at all.
- **Two tmux invocations to create a session and set its option is a window.** For that moment
  the brand-new session has tmux's default `update-environment`. `openWindow` chains both in one
  client call — `new-session … \; set-option -t <session> update-environment ''` — because a
  bare `;` argv element is tmux's command separator (the same rule that bit Design 2, used on
  purpose).
- **An object literal as a name set has a prototype chain.** `TMUX_OWN_GLOBALS[name]` was truthy
  for a table entry named `constructor`; `Object.hasOwn` is the lookup, and a test names
  `__proto__` through a `Map` because a literal `__proto__` key is swallowed by JavaScript itself.
- **A docstring that promises what the code cannot do is what the reviewer keys on.** The first
  name regex silently skipped legal names with `-`, `.`, `/` or `:` (dashed bash functions,
  npm's `//host/:_authToken`) while the docstring said "every name". Either widen the code or
  narrow the claim; never leave the gap.
- **A safety check is a code path with its own inputs.** Design 2's confirm step was added *as
  a safety check* and introduced the two worst behaviours of the whole PR (removing `HOME`;
  refusing boot on a fragment). Fuzz the check with the same adversarial inputs as the thing it
  guards.
- **Widen a name predicate with its anchors.** `isSecretLikeName` grew from `_API_KEY` to the
  credential-shaped family, case-insensitive; `_PAT` is anchored as `(?:_FILE)?$` so `PATH`
  survives. Check the allow-list and this box's `mise env --json` against the new predicate
  before landing it.

## How to test and prove this class of change

- **Fakes hold mutable tables and apply each `-u` with tmux's re-tokenisation** (strip a
  trailing `;`, restore `\;`), so the invariants are exercised against what tmux would actually
  do, not against an argv list. Every reviewer reproduction became a case: `HOME;=x` inside a
  value never touches `HOME`; `key = value` never throws; a real entry `X;` is unset as `X\;`.
- **Prove on a scratch server, never production:** `tmux -L <scratch> new-session -d …` forked
  under a planted environment (a PEM with adversarial body lines, dashed and `:`-containing
  names, `X;`, an empty value, 150 bulk names), then the real `TmuxRuntime.scrubServerEnvironment`
  through a driver; read the tables back, `show-environment -g HOME` still exit 0, and check that
  no planted value fragment appears in the removed list, the failure message, **or
  `show-messages`**. 162 names took ~1.9 s and 4 listings on 3.7c. Repeat under `LANG=C.UTF-8`
  and `LANG=C`. `kill-server` after.
- **Names only, mechanically:** every tmux error in `tmux.ts` goes through one `failure()` helper
  that carries stderr only; parse failures carry offsets only; tests assert the planted value
  strings are absent from every message.

## Related

- `spawned-process-environment-is-an-allow-list-and-the-tmux-tables-are-part-of-it.md` — why the tables are scrubbed at all.
- `a-failed-list-panes-proves-nothing-about-the-pane.md`, `tmux-list-panes-target-is-its-window.md` — other tmux command-surface traps in the same module.

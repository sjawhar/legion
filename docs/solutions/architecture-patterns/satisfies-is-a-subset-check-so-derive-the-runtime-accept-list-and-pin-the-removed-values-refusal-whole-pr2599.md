---
title: "satisfies is a subset check, so derive the runtime accept-list and pin the removed value's refusal whole"
category: architecture-patterns
tags:
  - typescript
  - enumeration
  - deletion
  - regression-test
  - acceptance-criteria
date: 2026-11-01
status: active
module: packages/envoy-client
related_issues:
  - "sjawhar/legion#2599"
  - "INFRA-429"
---

# `satisfies` is a subset check, so derive the runtime accept-list and pin the removed value's refusal whole

When you remove a member of a union type — a host, a harness, a backend, a provider — the
compiler does not find the runtime accept-list or the refusal message. `as const satisfies
readonly T[]` checks that every listed value **is** a `T`; it never checks that every `T` is
listed. So a list that still holds the removed value compiles, and a refusal that still names it
is a string nobody type-checks.

- **Derive the list from a value the type is computed from**, so one edit removes the member from
  the type, the list and every loop at once:

  ```ts
  const HARNESS_DIRECTORY_VARIABLE = { omp: "PI_CODING_AGENT_DIR", claude: "CLAUDE_CONFIG_DIR" } as const;
  type PluginHarness = keyof typeof HARNESS_DIRECTORY_VARIABLE;
  const PLUGIN_HARNESSES = Object.keys(HARNESS_DIRECTORY_VARIABLE) as readonly PluginHarness[];
  ```

- **Where the list cannot be derived, pin the removal with a test that asserts the refusal's whole
  message**, not its prefix, and prove the pin by mutation: put the removed value back and watch
  the test go red. A prefix assertion passes again the moment the value returns, and so does an
  assertion that only checks the exit code.
- **Pin the removed value, not a value that was never accepted.** A case sending a name the
  command never knew exercises the same branch and proves nothing about the removal; keep it, and
  add the removed one beside it.
- **Give a "no remaining mentions of X" acceptance criterion a keep-category for that test**, when
  you write the criterion. Otherwise the criterion argues against its own proof: the test must
  name X to pin X's removal, and the census then lists a file that fits none of the criterion's
  categories.

## Evidence

sjawhar/legion#2599 removed the `opencode` host from `packages/envoy-client`. Both patterns sat in
the same package:

- Derived, and safe: `src/envoy-config.ts:70-78` is the map, `keyof typeof` and `Object.keys`
  above. Deleting the one `opencode:` entry removed the harness from the type, from
  `PLUGIN_HARNESSES` and from the `command` harness's search order together; the file's whole diff
  is that entry plus prose.
- Restated three times, and not safe: `src/dispatch-cwd.ts:41` declares
  `export type DispatchHost = "omp" | "claude";` — a type with no runtime value;
  `src/dispatch-cli.ts:61` restates it as
  `const HOSTS = ["omp", "claude"] as const satisfies readonly DispatchHost[];`; and
  `src/dispatch-cli.ts:257` restates it a third time inside the refusal,
  `` `dispatch: DISPATCH_HOST is ${JSON.stringify(hostName)}, not omp or claude.` ``.

The reviewer measured that putting `"opencode"` back into `HOSTS` compiled and passed every test:
the only unknown-host cases sent `DISPATCH_HOST: "vim"` and asserted the exit code and the
`dispatch: ` prefix alone, and the one test that had sent `"opencode"` had been changed to
`"omp"`. That cost one review round and one implementing round.

The fix, `07e71d9c4adf`, is at `src/__tests__/dispatch-cli.test.ts:1509-1521`: it sends
`DISPATCH_HOST: "opencode"` with a session id and asserts
`expect(stdout).toBe('dispatch: DISPATCH_HOST is "opencode", not omp or claude.\n')`, exit 2 and
no request, with a comment saying why the whole line is pinned. The mutation was run: `HOSTS`
regaining `opencode` fails it with `Expected: 2, Received: 0`, while both `vim` cases stay green.

The criterion clash is the second half. That issue's acceptance criterion A1 allowed three
keep-categories for a surviving `opencode` mention — config-file guards, recorded third-party
fixtures, and comments about fields installed plugin versions still send — and a regression test
that names `opencode` to pin its refusal is none of them. The pull request had to record it as a
departure from the criterion's wording and send it to the architect to decide.

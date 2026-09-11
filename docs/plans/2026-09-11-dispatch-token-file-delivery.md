# Dispatch token delivery hardening — implementation plan (LEGION-6)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (one
> implementer, one PR against `sjawhar/legion`, branch `legion/LEGION-6`). Steps use `- [ ]` for
> tracking. Every ruling from a review round supersedes this document.

**Goal:** No Dispatch bearer, boot token, or controller secret ever appears as a tmux `-e KEY=VALUE`
argv value (world-readable `/proc/<pid>/cmdline`, finding F1), and every tmux command the daemon
runs targets a private server it forked itself (finding F2), so the pane-only invariant becomes
structural instead of aspirational.

**Architecture:** `tmux.ts` builds every argv as `tmux -L legion-<project> <subcommand> …`; the
server that the first such command forks inherits the daemon runner's already-stripped `paneEnv`.
Secrets move to `<state_dir>/secrets/` (directory 0700, files 0600): `dispatch-token` written once
at daemon startup, one file per pane named by that pane's role token
(`legion-<project>-<key>-<role>`, `legion-<project>-controller`) written immediately before the
pane's launch. Panes receive only `DISPATCH_TOKEN_FILE` / `LEGION_BOOT_TOKEN_FILE` /
`LEGION_CONTROLLER_SECRET_FILE` pointers. Every consumer resolves `X_FILE` (trimmed contents) ahead
of `X`, failing loudly on a set-but-unreadable or empty file, never falling back. Pane files are
pruned whenever the daemon persists state and no live locator references them any more.

**Tech stack:** TypeScript on Bun; `node:fs/promises`; Bun test; bash smoke rig
(`scripts/smoke/*.sh` with `*.test.sh` harnesses); tmux 3.7b (`-L socket-name`).

**Spec:** `dispatch://LEGION-6/spec` (version 3). Findings F1–F5:
`dispatch://LEGION-6/artifact/dispatch-token-hardening-md`.

## Decisions needed

None. Three refinements below deviate from the spec's Design table wording; the architect vetoes any
of them before Task 1 starts or they stand.

## Refinements to the spec (read before implementing)

1. **Pane secret file names are the role tokens, not `root-<KEY>` / `worker-<KEY>-<role>` /
   `controller`.** `<state_dir>/secrets/legion-omp-legion-42-architect`,
   `<state_dir>/secrets/legion-omp-controller`. Reason: the token is the one identity every
   launch, stop, and state entry already carries (`state.roles` keys, `stopProcess(token, …)`), so
   liveness of a file is derivable from state with no root/worker inference and no new field on
   `WorkerLocator` (the spec rejects locator schema churn). Acceptance 1's `<pane-key>` is read as
   the token; `secrets/controller` becomes `secrets/legion-<project>-controller`.
2. **Removal is derived, not sprinkled.** Fourteen sites clear a locator today; instead of a `rm` at
   each, `ProcessManager.persist()` runs `pruneSecretFiles()` after every save: every file in
   `secrets/` not named by a live locator (tree, worker claim, controller), the fixed
   `dispatch-token`, or an in-flight launch is removed. This is "removed when the daemon clears
   that pane's locator" (every clear is followed by a save) plus crash safety (a boot-time prune
   reaps leftovers) and race safety (a stale generation's stop can never delete a newer
   generation's same-named file, because the newer locator keeps it live).
3. **`kill-pane` reporting `no server running` counts as "pane already gone".** On the private
   socket, no server means no Legion pane exists anywhere; without this, the spec's own upgrade
   story (old panes on the default server → the daemon probes the private socket, finds them dead,
   resurrects) throws `StopFailed` on every retirement until the first `new-session` happens to
   create the private server. Three test fixtures that used `no server running` as the generic
   kill failure switch to `lost server`.
4. **The `dispatch-token` file is written by `startDaemonLocked` (`index.ts`), not by
   `resolveDaemonEnvironment`.** `resolveDaemonEnvironment` is injectable and replaced wholesale in
   `index.test.ts`; writing the file in `index.ts` means the daemon test harness exercises the real
   write. Same startup-refusal semantics as the spec: an fs error is thrown before any pane exists.
5. **`resolveDispatchConfig` reports a bad `DISPATCH_TOKEN_FILE` through its `error` channel**
   (`{enabled: false, error}`, surfaced by `envoy.ts` as a session-start warning and no dispatch
   tools), not by throwing — a throw from `envoyExtension(pi)`'s top level would take every Envoy
   tool down, not just Dispatch. It still never falls back to `DISPATCH_TOKEN` or `dispatch.token`.
   `legion.ts`/`classify.ts` (pi-envoy) and the daemon CLI *do* throw, as the spec says.

## Global constraints

- Socket name = session name = `legion-<project>` where `project` is
  `DaemonConfig.project` (`legionId.toLowerCase().replace(/[^a-z0-9]/g, "")`, `config.ts:646`); the
  smoke rig's `project_slug()` in `checkpoints.sh`/`down.sh` computes the same string.
- `-L <socket>` is a tmux *global* option and must precede the subcommand: argv shape is always
  `["tmux", "-L", "<socket>", "<subcommand>", …]`. Tests therefore read the subcommand at index 3.
- Secrets directory `<state_dir>/secrets` mode 0700; each file mode 0600; contents exactly the
  token, no trailing newline. Written before the tmux call that launches the pane that reads it.
- Pointer variables: `DISPATCH_TOKEN_FILE`, `LEGION_BOOT_TOKEN_FILE`,
  `LEGION_CONTROLLER_SECRET_FILE`. The daemon never emits `DISPATCH_TOKEN`, `LEGION_BOOT_TOKEN`, or
  `LEGION_CONTROLLER_SECRET` to any pane.
- Consumer precedence: `X_FILE` set → trimmed file contents, error if unreadable/empty (naming the
  variable and the path); `X_FILE` unset → `X` exactly as today. `X_FILE` and `X` both set →
  `X_FILE` wins silently.
- Option 3 (per-pane redeemable Dispatch tokens) is out of scope. No `LegionState` schema change.
- Repo rules: `node:` imports, Biome (double quotes, semicolons, 100 cols), `type` imports, no
  barrel files, co-located `__tests__/`, comments describe current behaviour, jj not git. Run
  `bunx biome check`, `bunx tsc --noEmit`, `bun test` per package only at the end of a task; never
  `| head` / `| tail` on gate commands.
- Every commit: `cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" commit -m "<message>"`
  (`jj commit` = describe + new). Never `jj abandon`, never `jj edit @-`.

## Upgrade on a live box (operator runbook — goes into daemon AGENTS.md in Task 8)

1. `legion stop <team>` (or stop the supervisor).
2. `tmux kill-session -t legion-<slug>` on the **default** server (the pre-upgrade panes).
3. Start the new daemon. `reconnectWorkers` finds every recorded socket dead and retires the
   claims; `kill-pane` against the not-yet-running private server reports `no server running`,
   which refinement 3 treats as gone; roots resurrect with `--resume` onto the private server.
4. Attach with `tmux -L legion-<slug> attach -t legion-<slug>`.

## File structure

| file | responsibility after this plan |
| :--- | :--- |
| `packages/daemon/src/daemon/secrets.ts` (new) | `secretsDir`, `secretFilePath`, `writeSecretFile` (0700 dir, 0600 file), `pruneSecretFiles(stateDir, keep)`, `DISPATCH_TOKEN_SECRET`. No daemon state. |
| `packages/daemon/src/daemon/tmux.ts` | `TmuxServer {run, socket}`; every exported function takes it and builds `tmux -L <socket> …`. |
| `packages/daemon/src/daemon/worker-boot-watchdog.ts` | dep `run: TmuxRun` → `tmux: TmuxServer`. |
| `packages/daemon/src/daemon/processes.ts` | `this.tmux`; pane secret write in `launchShimmedProcess`/`spawnController`; `_FILE` pointers; `persist()` prunes; `stopProcess` gone-regex. |
| `packages/daemon/src/daemon/index.ts` | writes `dispatch-token` at startup; boot-time prune. |
| `packages/daemon/src/daemon/environment.ts` | `stripDispatchEnv` also strips `DISPATCH_TOKEN_FILE`; doc comment states the file-pointer invariant. |
| `packages/daemon/src/cli/index.ts` | `controllerSecret()` honours `LEGION_CONTROLLER_SECRET_FILE`. |
| `packages/envoy-client/src/secret-file.ts` (new) | `readSecretFile(variable, path)`: trimmed contents or a descriptive throw. Exported as `@legion/envoy-client/secret-file`. |
| `packages/envoy-client/src/dispatch-config.ts` | token precedence `DISPATCH_TOKEN_FILE` → `DISPATCH_TOKEN` → `dispatch.token`. |
| `packages/pi-envoy/src/legion/classify.ts` | `requiredSecret(env, key)`; `requiredControllerCapability` honours `_FILE`. |
| `packages/pi-envoy/extensions/legion.ts` | three boot-token reads go through `requiredSecret`. |
| `scripts/smoke/checkpoints.sh`, `down.sh`, `*.test.sh`, `README.md` | `legion_tmux` helper (`-L`), checkpoint 13, `<1-13>`. |
| Docs | `packages/daemon/src/daemon/AGENTS.md`, `packages/envoy-client/README.md`, `packages/pi-envoy/AGENTS.md`, `packages/pi-envoy/README.md`, `skills/legion-worker/SKILL.md`, `skills/legion-controller/SKILL.md`. |

Task order: 1 → 2 → 3 → 4 (daemon, sequential: 2 and 3 both edit `processes.ts`/`processes.test.ts`),
then 5 → 6 (pi-envoy imports the envoy-client helper), 7 (smoke), 8 (docs + gates + PR). Tasks 4, 5,
and 7 are independent of each other.

---

### Task 1: `secrets.ts` — the file primitives

**Files:**
- Create: `packages/daemon/src/daemon/secrets.ts`
- Create: `packages/daemon/src/daemon/__tests__/secrets.test.ts`

**Interfaces — produces:**
```ts
export const DISPATCH_TOKEN_SECRET = "dispatch-token";
export function secretsDir(stateDir: string): string;                       // <stateDir>/secrets
export function secretFilePath(stateDir: string, name: string): string;     // <stateDir>/secrets/<name>
export async function writeSecretFile(stateDir: string, name: string, value: string): Promise<string>; // returns the path
export async function pruneSecretFiles(stateDir: string, keep: ReadonlySet<string>): Promise<string[]>; // returns removed names
```

- [ ] **Step 1: Write the failing tests**

```ts
// packages/daemon/src/daemon/__tests__/secrets.test.ts
import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { mkdtemp, readdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  DISPATCH_TOKEN_SECRET,
  pruneSecretFiles,
  secretFilePath,
  secretsDir,
  writeSecretFile,
} from "../secrets";

let stateDir: string;

beforeEach(async () => {
  stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-secrets-"));
});

afterEach(async () => {
  await rm(stateDir, { recursive: true, force: true });
});

describe("writeSecretFile", () => {
  it("creates <stateDir>/secrets as 0700 and the file as 0600 holding exactly the value", async () => {
    const file = await writeSecretFile(stateDir, "legion-omp-controller", "controller-secret");

    expect(file).toBe(secretFilePath(stateDir, "legion-omp-controller"));
    expect((await stat(secretsDir(stateDir))).mode & 0o777).toBe(0o700);
    expect((await stat(file)).mode & 0o777).toBe(0o600);
    expect(await readFile(file, "utf8")).toBe("controller-secret");
  });

  it("overwrites an existing file in place, restoring 0600 even if the mode had drifted", async () => {
    const file = await writeSecretFile(stateDir, "legion-omp-controller", "first");
    await writeFile(file, "tampered", { mode: 0o644 });
    await (await import("node:fs/promises")).chmod(file, 0o644);

    await writeSecretFile(stateDir, "legion-omp-controller", "second");

    expect(await readFile(file, "utf8")).toBe("second");
    expect((await stat(file)).mode & 0o777).toBe(0o600);
  });
});

describe("pruneSecretFiles", () => {
  it("removes every file not in keep and reports the removed names", async () => {
    await writeSecretFile(stateDir, DISPATCH_TOKEN_SECRET, "dispatch");
    await writeSecretFile(stateDir, "legion-omp-legion-42-architect", "root");
    await writeSecretFile(stateDir, "legion-omp-legion-43-tester", "stale");

    const removed = await pruneSecretFiles(
      stateDir,
      new Set([DISPATCH_TOKEN_SECRET, "legion-omp-legion-42-architect"])
    );

    expect(removed).toEqual(["legion-omp-legion-43-tester"]);
    expect((await readdir(secretsDir(stateDir))).sort()).toEqual(
      [DISPATCH_TOKEN_SECRET, "legion-omp-legion-42-architect"].sort()
    );
  });

  it("is a no-op when the secrets directory does not exist yet", async () => {
    expect(await pruneSecretFiles(stateDir, new Set())).toEqual([]);
  });
});
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd -- "$LEGION_WORKSPACE/packages/daemon" && bun test src/daemon/__tests__/secrets.test.ts`
Expected: FAIL — `Cannot find module '../secrets'`.

- [ ] **Step 3: Implement**

```ts
// packages/daemon/src/daemon/secrets.ts
import { chmod, mkdir, readdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";

/** The one Dispatch bearer every pane shares, written once at daemon startup (`index.ts`). */
export const DISPATCH_TOKEN_SECRET = "dispatch-token";

/** `<stateDir>/secrets`: every secret a pane reads lives here as a 0600 file inside a 0700
 * directory, and a pane receives only the file's path (`DISPATCH_TOKEN_FILE`,
 * `LEGION_BOOT_TOKEN_FILE`, `LEGION_CONTROLLER_SECRET_FILE`) on its tmux `-e` argv — never the
 * value, which would otherwise sit in the world-readable `/proc/<pid>/cmdline` of the transient
 * tmux client for as long as it runs. Pane files are named by the pane's role token
 * (`legion-<project>-<key>-<role>`, `legion-<project>-controller`), the same key `state.roles`
 * uses, so `ProcessManager.pruneSecretFiles` can derive which ones a live locator still needs. */
export function secretsDir(stateDir: string): string {
  return path.join(stateDir, "secrets");
}

export function secretFilePath(stateDir: string, name: string): string {
  return path.join(secretsDir(stateDir), name);
}

/** Writes `value` to `<stateDir>/secrets/<name>` (created or overwritten in place) and returns that
 * path. Re-applies 0700/0600 explicitly on every call: `mkdir`'s mode is umask-masked and ignored
 * for an existing directory, and `writeFile`'s mode applies only on create. */
export async function writeSecretFile(
  stateDir: string,
  name: string,
  value: string
): Promise<string> {
  const dir = secretsDir(stateDir);
  await mkdir(dir, { recursive: true, mode: 0o700 });
  await chmod(dir, 0o700);
  const file = path.join(dir, name);
  await writeFile(file, value, { encoding: "utf8", mode: 0o600 });
  await chmod(file, 0o600);
  return file;
}

/** Removes every entry of `<stateDir>/secrets` whose name is not in `keep`, returning the removed
 * names. A missing directory is an empty one. */
export async function pruneSecretFiles(
  stateDir: string,
  keep: ReadonlySet<string>
): Promise<string[]> {
  let names: string[];
  try {
    names = await readdir(secretsDir(stateDir));
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return [];
    throw error;
  }
  const removed = names.filter((name) => !keep.has(name)).sort();
  await Promise.all(removed.map((name) => rm(secretFilePath(stateDir, name), { force: true })));
  return removed;
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd -- "$LEGION_WORKSPACE/packages/daemon" && bun test src/daemon/__tests__/secrets.test.ts`
Expected: 4 pass.

- [ ] **Step 5: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" commit -m "feat(daemon): secrets.ts — 0700 dir / 0600 file primitives for pane secrets"
```

---

### Task 2: private tmux socket — `tmux -L legion-<project>` on every command

**Files:**
- Modify: `packages/daemon/src/daemon/tmux.ts` (all exported functions)
- Modify: `packages/daemon/src/daemon/worker-boot-watchdog.ts:3-4,38,178`
- Modify: `packages/daemon/src/daemon/processes.ts` (`this.tmux`; 13 `tmux.*(this.deps.run, …)` call sites; `:1217`, `:2345`, `:2558` session strings; `:1329` direct `run`; `:2735` gone-regex)
- Modify tests: `__tests__/processes.test.ts`, `__tests__/processes.thermo-ops.test.ts`, `__tests__/index.test.ts`, `__tests__/worker-boot-watchdog.test.ts`, `__tests__/real-shutdown-e2e.test.ts`

**Interfaces — produces:**
```ts
// tmux.ts
export interface TmuxServer { readonly run: TmuxRun; readonly socket: string; }
export async function openWindow(server: TmuxServer, session, name, environmentAndCommand, owner)
export async function splitWindow(server: TmuxServer, windowId, environmentAndCommand)
export async function windowAlive(server: TmuxServer, windowId)
export async function panePid(server: TmuxServer, target)
export async function firstPaneId(server: TmuxServer, windowId)
export async function killWindow(server: TmuxServer, windowId)
export async function killPane(server: TmuxServer, paneId)
export async function listUnknownOwnedWindows(server: TmuxServer, session, owner, known)
export async function listUnknownPanes(server: TmuxServer, owner, known)
// processes.ts
private readonly tmux: tmux.TmuxServer;   // { run: deps.run, socket: `legion-${deps.state.project}` }
```

- [ ] **Step 1: Write the failing test (argv shape) in `processes.test.ts`**

Append inside the main `describe` (near the existing "launches controller and root windows with
disjoint exact environments" test, `:629`):

```ts
  it("runs every tmux command against the private legion-<project> socket", async () => {
    const stateDir = await temporaryDir();
    let sessionExists = false;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir),
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    await processes.spawnRoot(root);
    await processes.reconcileTmuxWindows();

    const tmuxCommands = commands.filter((command) => command[0] === "tmux");
    expect(tmuxCommands.length).toBeGreaterThan(0);
    for (const command of tmuxCommands) {
      expect(command.slice(0, 3)).toEqual(["tmux", "-L", "legion-omp"]);
    }
    expect(tmuxCommands.map((command) => command[3])).toContain("list-windows");
  });
```

Also add, next to the `no server running` fixture test at `:2534`:

```ts
  it("treats a kill-pane that finds no server on the private socket as an already-gone pane", async () => {
    const state = newLegionState("omp", 1);
    tree(state);
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const { manager: processes } = manager(state, {
      sleep: async () => {},
      run: async (command) => {
        if (command[0] === "tmux" && command[3] === "list-panes" && command.includes("#{pane_pid}")) {
          return { stdout: "", exitCode: 1 };
        }
        if (command[0] === "tmux" && command[3] === "kill-pane") {
          return {
            stdout: "",
            stderr: "no server running on /tmp/tmux-1000/legion-omp",
            exitCode: 1,
          };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.closeTree(root);

    expect(state.trees[root]?.status).toBe("closed");
    expect(state.trees[root]?.locator).toBeUndefined();
    errorLog.mockRestore();
  });
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd -- "$LEGION_WORKSPACE/packages/daemon" && bun test src/daemon/__tests__/processes.test.ts -t "private legion-<project> socket|no server on the private socket"`
Expected: FAIL (argv starts `["tmux", "new-window"…]`; second rejects with `StopFailed`).

- [ ] **Step 3: Rewrite `tmux.ts` around `TmuxServer`**

Add after the `TmuxRun` type (`tmux.ts:7`):

```ts
/** The private tmux server this daemon owns. Every argv this module builds starts
 * `tmux -L <socket>`, so the server is forked by the daemon's own first command and inherits the
 * runner's stripped `paneEnv` (never a human's shell that may carry `DISPATCH_TOKEN`), and no
 * Legion pane ever shares a server with the operator's own sessions. The socket name equals the
 * session name (`legion-<project>`): attach with `tmux -L legion-<project> attach -t legion-<project>`. */
export interface TmuxServer {
  readonly run: TmuxRun;
  readonly socket: string;
}

function argv(server: TmuxServer, ...rest: string[]): string[] {
  return ["tmux", "-L", server.socket, ...rest];
}
```

Then, in every function, replace the first parameter `run: TmuxRun` with `server: TmuxServer` and
every `run([ "tmux", <rest…> ])` with `server.run(argv(server, <rest…>))`. Concretely (same bodies
otherwise):

| function | before | after |
| :--- | :--- | :--- |
| `markOwner` | `run(["tmux", "set-option", ...(scope === "window" ? ["-w"] : []), "-t", target, "@legion_owner", owner])`; kill fallback `run(["tmux", "kill-window", "-t", target])` | `server.run(argv(server, "set-option", ...(scope === "window" ? ["-w"] : []), "-t", target, "@legion_owner", owner))`; `server.run(argv(server, "kill-window", "-t", target))` |
| `openWindow` | `has-session`, `new-session … "sleep 3600"`, `new-window -P -F … -t session -n name …env`, bootstrap `kill-window`, `markOwner(run, …)` | all via `argv(server, …)`; `markOwner(server, …)` |
| `splitWindow` | `split-window`, `select-layout` | `argv(server, …)` |
| `windowAlive`, `panePid`, `firstPaneId` | `list-panes …` | `argv(server, …)` |
| `killWindow`, `killPane` | `kill-window` / `kill-pane` | `argv(server, …)` |
| `listUnknownOwnedWindows` | `list-windows -t session -F …` | `argv(server, …)` |
| `listUnknownPanes` | `list-panes -a -F …` | `argv(server, …)` |

Fully written example (the others follow the same mechanical shape):

```ts
export async function killPane(
  server: TmuxServer,
  paneId: string
): Promise<{ exitCode: number; stderr?: string }> {
  const result = await server.run(argv(server, "kill-pane", "-t", paneId));
  return { exitCode: result.exitCode, stderr: result.stderr };
}
```

Update the `listUnknownPanes` doc comment's "anywhere on the tmux server" to "anywhere on this
daemon's private tmux server".

- [ ] **Step 4: Thread `TmuxServer` through `worker-boot-watchdog.ts`**

`worker-boot-watchdog.ts:38` `run: TmuxRun;` → `tmux: TmuxServer;` (import
`type TmuxServer` instead of `type TmuxRun`); `:178`
`tmux.panePid(this.deps.run, target)` → `tmux.panePid(this.deps.tmux, target)`.

- [ ] **Step 5: Wire `processes.ts`**

Add a field after `bootWatchdog` (`processes.ts:270`):

```ts
  /** The private tmux server this daemon owns — see `TmuxServer`. Socket and session share the
   * `legion-<project>` name. */
  private readonly tmux: tmux.TmuxServer;
```

In the constructor, before `this.workerAdmission = …`:

```ts
    this.tmux = { run: deps.run, socket: `legion-${deps.state.project}` };
```

and in the `WorkerBootWatchdog` deps replace `run: this.deps.run,` with `tmux: this.tmux,`.

Replace the three `const session = \`legion-${this.deps.state.project}\`;` (`:1217`, `:2345`,
`:2558`) with `const session = this.tmux.socket;`.

Replace every `tmux.<fn>(this.deps.run, …)` with `tmux.<fn>(this.tmux, …)` — 13 sites: `:1231`,
`:1239`, `:1254`, `:1258`, `:1645`, `:1649`, `:2182`, `:2352`, `:2358`, `:2571`, `:2734`, `:2799`,
`:2810` (`rg -n 'tmux\.[a-zA-Z]+\(this\.deps\.run' packages/daemon/src/daemon/processes.ts` must
print nothing afterwards).

Replace the direct call at `:1329`:

```ts
        await this.deps.run(["tmux", "kill-window", "-t", tree.locator.tmuxWindowId]);
```
with
```ts
        await tmux.killWindow(this.tmux, tree.locator.tmuxWindowId);
```
(`rg -n '"tmux"' packages/daemon/src/daemon/processes.ts` must print nothing afterwards.)

Change the gone-regex in `stopProcess` (`:2735`):

```ts
    if (killed.exitCode !== 0 && !/can't find pane|no server running/.test(killed.stderr ?? "")) {
```
and extend the doc comment above `stopProcess` (`:2700-2704`): "…other than the pane having already
been reaped on its own (`"can't find pane"`) or the private server itself not running
(`"no server running"` — no server on this daemon's own socket means no Legion pane exists) —".

- [ ] **Step 6: Codemod the daemon tests**

Population (verified 2026-09-11): `processes.test.ts` 314 `command[1] === "<tmux-subcommand>"`
comparisons, 12 `c[1] === …`, 23 single-line `["tmux", "` literals, 5 multi-line `"tmux",` array
heads, 9 tmux index reads at `:1547,1583,1587,1775,9248,9272,9316,9344,9380`;
`processes.thermo-ops.test.ts` 4 comparisons + 1 literal + 1 multi-line head; `index.test.ts` 11
comparisons (`command[0]?.endsWith("/tmux") && command[1] === …`), 0 literals. The three
`no server running` fixtures are at `processes.test.ts:2555,2630,5698`.

Run, in `packages/daemon/src/daemon/__tests__`:

```bash
# 1. tmux index reads that shift by two (do these BEFORE the subcommand sed, which creates new command[3] uses)
sed -i -E 's/killedWindows\.push\(command\[3\] \?\? ""\)/killedWindows.push(command[5] ?? "")/g' processes.test.ts
# then hand-edit: :1583  command[2] === "-w"            → command[4] === "-w"
#                 :9248, :9316  command[2] === "-a"    → command[4] === "-a"
#                 :9272, :9344  .map((command) => command[3])   → command[5]   (these read the -t target of a tmux argv; confirm by reading the surrounding filter)
#                 :9380  command[2] === "-t" ? command[3]        → command[4] === "-t" ? command[5]
# 2. subcommand comparisons
sed -i -E 's/command\[1\] (===|!==) "(has-session|new-session|new-window|split-window|kill-window|kill-pane|list-panes|list-windows|set-option|select-layout)"/command[3] \1 "\2"/g' processes.test.ts processes.thermo-ops.test.ts index.test.ts
sed -i -E 's/\bc\[1\] (===|!==) "(new-window|split-window|kill-pane|kill-window)"/c[3] \1 "\2"/g' processes.test.ts
# 3. single-line literals (project "omp" in both files)
sed -i -E 's/\["tmux", "/["tmux", "-L", "legion-omp", "/g' processes.test.ts processes.thermo-ops.test.ts
# 4. generic kill failure fixture
sed -i -E 's/stderr: "no server running"/stderr: "lost server"/g' processes.test.ts
```

Then hand-edit the 6 multi-line array heads (`rg -n '^\s*"tmux",$' processes.test.ts processes.thermo-ops.test.ts`):
insert two lines `"-L",` and `"legion-omp",` after each `"tmux",`.

`index.test.ts` uses project `acme1`; its runner matches `command[0]?.endsWith("/tmux")` (no literal
arrays), so the sed above is sufficient there.

`worker-boot-watchdog.test.ts:88,207,241,273`: replace each `run: <fn>,` dep with
`tmux: { socket: "legion-omp", run: <fn> },` (the fakes match `cmd.includes("list-panes")`, so they
keep working).

`real-shutdown-e2e.test.ts`: add `const TMUX_SOCKET = "legion-realshutdown";` beside `SESSION` and
a helper `const tmuxArgv = (...rest: string[]) => ["tmux", "-L", TMUX_SOCKET, ...rest];`; rewrite
`ensureSession` (`:70,72-82`), `openShimWindow` (`:98-108`), `paneAlive` (`:128`), and `afterAll`
(`:229`, use `kill-server` instead of `kill-session`) to use `tmuxArgv(...)`. The test's
`config()` has `project: "realshutdown"`, so the `ProcessManager` under test kills panes on exactly
that socket. The assertion at `:417` (`command.includes("kill-pane")`) is unaffected.

- [ ] **Step 7: Run the daemon tests**

Run: `cd -- "$LEGION_WORKSPACE/packages/daemon" && bun test`
Expected: all pass, including the two new tests. If any `expect(...).toEqual([["tmux", …` still
fails, the argv differs only by the missing `"-L", "legion-omp"` — fix the literal, never the
production code. Then `LEGION_E2E=1 bun test src/daemon/__tests__/real-shutdown-e2e.test.ts`
(needs a real `tmux` on PATH) → pass; afterwards
`tmux -L legion-realshutdown list-sessions` must report `no server running`.

- [ ] **Step 8: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" commit -m "feat(daemon): every tmux command targets the private legion-<project> socket"
```

---

### Task 3: pane secrets as files — daemon side

**Files:**
- Modify: `packages/daemon/src/daemon/processes.ts` (`:172-177` `tmuxEnv` unchanged; imports; new
  fields; `launchShimmedProcess :2329-2371`; `spawnTree :2002-2049`; `launchWorker :2403-2432`;
  `spawnController :2550-2585`; `persist :2856`; 17 `this.deps.saveState()` sites)
- Modify: `packages/daemon/src/daemon/index.ts:350-354,422-426`
- Modify: `packages/daemon/src/daemon/environment.ts:90-103`
- Modify tests: `__tests__/processes.test.ts` (`:561-613`, `:654-682`, `:3395-3427`, `:5290-5300`,
  `:8292-8348`, plus new), `__tests__/index.test.ts` (`:160-164`, `config()` `:217-245`),
  `__tests__/environment.test.ts:157-206`

**Interfaces — consumes:** Task 1's `secrets.ts`; Task 2's `this.tmux`.
**Interfaces — produces:** pane env keys `DISPATCH_TOKEN_FILE`, `LEGION_BOOT_TOKEN_FILE`,
`LEGION_CONTROLLER_SECRET_FILE`; `ProcessManager.pruneSecretFiles(): Promise<void>` (public, called
by `index.ts` at boot); `this.persist()` = save + prune.

- [ ] **Step 1: Write the failing acceptance test (spec Acceptance 1) in `processes.test.ts`**

Add `readFile` and `stat` to the `node:fs/promises` import if absent. Then:

```ts
  it("delivers every pane secret as a 0600 file pointer inside a 0700 secrets dir, never as a -e value", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    let sessionExists = false;
    const { manager: processes, commands } = manager(newLegionState("omp", 1), {
      config: config(stateDir, {
        dispatchUrl: "http://127.0.0.1:18766",
        dispatchToken: "test-dispatch-token",
      }),
      mintControllerCapability: async () => "controller-secret",
      mintBootToken: async () => "root-boot-token",
      mintWorkerBootToken: async () => "worker-boot-token",
      run: async (command) => {
        commands.push(command);
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") {
          return { stdout: `@${commands.length} %${commands.length} 12345\n`, exitCode: 0 };
        }
        if (command[3] === "split-window") {
          return { stdout: `%${commands.length} 12345\n`, exitCode: 0 };
        }
        return { stdout: "", exitCode: 0 };
      },
    });

    await processes.ensureController();
    await processes.spawnRoot(root);
    await processes.spawnWorker(root, root, "tester", "verify #41");

    const secrets = ["test-dispatch-token", "root-boot-token", "worker-boot-token", "controller-secret"];
    const tmuxCommands = commands.filter((command) => command[0] === "tmux");
    for (const command of tmuxCommands) {
      expect(command.slice(0, 3)).toEqual(["tmux", "-L", "legion-omp"]);
      for (const part of command) for (const secret of secrets) expect(part).not.toContain(secret);
    }
    const launches = tmuxCommands.filter((c) => c[3] === "new-window" || c[3] === "split-window");
    expect(launches).toHaveLength(3);
    const [controller, architect, tester] = launches.map(tmuxWindowEnvironment);
    if (!controller || !architect || !tester) throw new Error("missing launches");
    const dir = path.join(stateDir, "secrets");
    expect(controller).toMatchObject({
      DISPATCH_TOKEN_FILE: path.join(dir, "dispatch-token"),
      LEGION_CONTROLLER_SECRET_FILE: path.join(dir, "legion-omp-controller"),
    });
    expect(architect).toMatchObject({
      DISPATCH_TOKEN_FILE: path.join(dir, "dispatch-token"),
      LEGION_BOOT_TOKEN_FILE: path.join(dir, roleToken("omp", root, "architect")),
    });
    expect(tester).toMatchObject({
      DISPATCH_TOKEN_FILE: path.join(dir, "dispatch-token"),
      LEGION_BOOT_TOKEN_FILE: path.join(dir, roleToken("omp", root, "tester")),
    });
    for (const environment of [controller, architect, tester]) {
      expect(environment.DISPATCH_TOKEN).toBeUndefined();
      expect(environment.LEGION_BOOT_TOKEN).toBeUndefined();
      expect(environment.LEGION_CONTROLLER_SECRET).toBeUndefined();
    }
    expect((await stat(dir)).mode & 0o777).toBe(0o700);
    for (const [file, value] of [
      [controller.LEGION_CONTROLLER_SECRET_FILE, "controller-secret"],
      [architect.LEGION_BOOT_TOKEN_FILE, "root-boot-token"],
      [tester.LEGION_BOOT_TOKEN_FILE, "worker-boot-token"],
    ] as const) {
      if (!file) throw new Error("pointer missing");
      expect((await stat(file)).mode & 0o777).toBe(0o600);
      expect(await readFile(file, "utf8")).toBe(value);
    }
  });

  it("prunes a pane's secret file once no locator references it, keeping the others", async () => {
    const stateDir = await temporaryDir();
    const workspace = path.join(stateDir, "workspaces", "sjawhar", "legion", "issue-42");
    await mkdir(workspace, { recursive: true });
    let sessionExists = false;
    const { manager: processes } = manager(newLegionState("omp", 1), {
      config: config(stateDir, {
        dispatchUrl: "http://127.0.0.1:18766",
        dispatchToken: "test-dispatch-token",
      }),
      sleep: async () => {},
      run: async (command) => {
        if (command[3] === "has-session") return { stdout: "", exitCode: sessionExists ? 0 : 1 };
        if (command[3] === "new-session") sessionExists = true;
        if (command[3] === "new-window") return { stdout: "@42 %1 12345\n", exitCode: 0 };
        if (command[3] === "split-window") return { stdout: "%2 12345\n", exitCode: 0 };
        return { stdout: "", exitCode: 0 };
      },
    });
    await processes.ensureController();
    await processes.spawnRoot(root);
    await processes.spawnWorker(root, root, "tester", "verify #41");
    const dir = path.join(stateDir, "secrets");
    const architectFile = path.join(dir, roleToken("omp", root, "architect"));
    const testerFile = path.join(dir, roleToken("omp", root, "tester"));
    expect((await readdir(dir)).sort()).toEqual(
      [roleToken("omp", root, "architect"), roleToken("omp", root, "tester"), "legion-omp-controller"].sort()
    );

    await processes.markProcessDead(root);

    expect(await stat(architectFile).catch(() => undefined)).toBeUndefined();
    expect(await readFile(testerFile, "utf8")).toBe("worker-boot-token");

    await processes.closeTree(root);

    expect((await readdir(dir)).sort()).toEqual(["legion-omp-controller"]);
  });
```

(`readdir` from `node:fs/promises`; `manager()`'s default `mintWorkerBootToken` returns
`"worker-boot-token"` — check `:224-330` and pass it explicitly if not. `dispatch-token` is written
by `index.ts` at startup, not by `ProcessManager`, so it is absent from these directories.)

- [ ] **Step 2: Run to verify they fail**

Run: `cd -- "$LEGION_WORKSPACE/packages/daemon" && bun test src/daemon/__tests__/processes.test.ts -t "0600 file pointer|prunes a pane's secret file"`
Expected: FAIL (`DISPATCH_TOKEN=test-dispatch-token` present; no `secrets/` directory).

- [ ] **Step 3: Implement in `processes.ts`**

Imports: add `import { DISPATCH_TOKEN_SECRET, pruneSecretFiles, secretFilePath, writeSecretFile } from "./secrets";`.

Fields (after `private readonly tmux`):

```ts
  /** `<state_dir>/secrets/dispatch-token`, written by `index.ts` at startup whenever
   * `config.dispatchToken` is set; exported to every pane as `DISPATCH_TOKEN_FILE`. */
  private readonly dispatchTokenFile: string | undefined;
  /** Role tokens whose pane secret file has been written for a launch that has not yet stored its
   * locator in state. `pruneSecretFiles` treats them as live so a persist racing the launch
   * (another role's save) cannot reap a file the pane is about to read. */
  private readonly launchingSecrets = new Set<string>();
```

Constructor: `this.dispatchTokenFile = deps.config.dispatchToken === undefined ? undefined : secretFilePath(deps.config.stateDir, DISPATCH_TOKEN_SECRET);`

New private helpers (place next to `persist()` at the end of the class):

```ts
  /** Writes `<state_dir>/secrets/<token>` for a pane about to launch and keeps it exempt from
   * pruning until `body` resolves. Callers store the locator `body` returns into state
   * synchronously (no `await` in between), so by the next `persist()` the file is referenced by a
   * live locator; a `body` that throws leaves the file for that persist's prune to reap. */
  private async withPaneSecret<T>(
    token: string,
    value: string,
    body: (secretFile: string) => Promise<T>
  ): Promise<T> {
    this.launchingSecrets.add(token);
    try {
      const secretFile = await writeSecretFile(this.deps.config.stateDir, token, value);
      return await body(secretFile);
    } finally {
      this.launchingSecrets.delete(token);
    }
  }

  /** Every secret file some live process still needs: the shared Dispatch bearer, one per tree
   * root with a locator, one per worker claim with a locator, the controller's when it has a
   * locator, and every launch currently in flight. */
  private liveSecretFiles(): Set<string> {
    const project = this.deps.state.project;
    const live = new Set<string>([DISPATCH_TOKEN_SECRET, ...this.launchingSecrets]);
    if (this.deps.state.controllerLocator) live.add(controllerToken(project));
    for (const tree of Object.values(this.deps.state.trees)) {
      if (tree.locator) live.add(roleToken(project, tree.root, "architect"));
    }
    for (const [token, claim] of Object.entries(this.deps.state.roles)) {
      if ("issue" in claim && claim.locator) live.add(token);
    }
    return live;
  }

  /** Removes every pane secret file no live locator references. Best-effort and never throws:
   * a prune failure is hygiene, not state, and must never turn a successful save into a failed
   * one. Runs after every `persist()` and once at boot (`index.ts`). */
  async pruneSecretFiles(): Promise<void> {
    try {
      await pruneSecretFiles(this.deps.config.stateDir, this.liveSecretFiles());
    } catch (error) {
      console.error("[legion] failed to prune pane secret files:", error);
    }
  }

  private async persist(): Promise<void> {
    await this.deps.saveState();
    await this.pruneSecretFiles();
  }
```

Replace all 17 `await this.deps.saveState();` / `this.deps.saveState()` occurrences in
`processes.ts` with `this.persist()` (`rg -n 'this\.deps\.saveState\(\)' packages/daemon/src/daemon/processes.ts`
must then print only the one inside `persist()`). The `saveState` catch blocks in `spawnRoot`
(`:1325-1332`) and `launchWorker` (`:2497-2523`) keep their semantics: `pruneSecretFiles` never
throws, so only a real save failure reaches them.

`launchShimmedProcess` (`:2329`): add parameter `bootToken: string` after `envPairs: string[]`;
compute `const token = roleToken(this.deps.state.project, issue, role);` and wrap the serialized
launch:

```ts
    const session = this.tmux.socket;
    const token = roleToken(this.deps.state.project, issue, role);
    const { tmuxWindowId, tmuxPaneId } = await this.withPaneSecret(token, bootToken, (bootTokenFile) =>
      this.serialize(this.issueLaunchQueue, issue, async () => {
        const pairs = [...envPairs, ...tmuxEnv({ LEGION_BOOT_TOKEN_FILE: bootTokenFile })];
        const existingWindowId = await this.probedWindowId(issue);
        if (existingWindowId) {
          const { paneId } = await tmux.splitWindow(this.tmux, existingWindowId, [
            ...pairs,
            shellCommand,
          ]);
          return { tmuxWindowId: existingWindowId, tmuxPaneId: paneId };
        }
        const window = await tmux.openWindow(
          this.tmux,
          session,
          treeName(issue),
          [...pairs, shellCommand],
          session
        );
        this.rewriteIssueWindowId(issue, window.windowId);
        return { tmuxWindowId: window.windowId, tmuxPaneId: window.paneId };
      })
    );
```

Update its doc comment: "…Writes the pane's boot token to `<state_dir>/secrets/<role token>`
first and exports only `LEGION_BOOT_TOKEN_FILE`; the write happens before any tmux call, so an fs
failure is an ordinary launch failure."

`spawnTree` (`:2002-2022`): delete the `LEGION_BOOT_TOKEN: bootToken,` line; replace
`DISPATCH_TOKEN: this.deps.config.dispatchToken,` with `DISPATCH_TOKEN_FILE: this.dispatchTokenFile,`;
pass `bootToken` as the new argument after `env` in the `launchShimmedProcess` call (`:2040-2049`).

`launchWorker` (`:2403-2432`): same two edits on the record; pass `bootToken` after `env`.

`spawnController` (`:2550-2585`) becomes:

```ts
  private async spawnController(controllerSecret: string): Promise<void> {
    const controllerDir = path.join(this.deps.config.stateDir, "controller");
    const promptPath = path.join(EXTENSION_PACKAGE, "roles", "controller-root.md");
    await (this.deps.statPrompt ?? stat)(promptPath);
    await this.writeOmpConfig(controllerDir);
    const socketPath = await this.prepareSocket("controller");
    const innerCommand = `${withOmpLaunchPrefix(this.deps.config.ompLaunchPrefix, this.deps.ompInvocation)} --mode rpc --append-system-prompt "$(cat ${shellPath(promptPath)})"`;
    const shellCommand = this.shimmedShellCommand(controllerDir, socketPath, innerCommand);
    const session = this.tmux.socket;
    const token = controllerToken(this.deps.state.project);
    await this.withPaneSecret(token, controllerSecret, async (secretFile) => {
      const env = tmuxEnv({
        LEGION_CONTROLLER: "1",
        LEGION_ROLE: "controller",
        LEGION_CONTROLLER_SECRET_FILE: secretFile,
        LEGION_DAEMON_URL: `http://127.0.0.1:${this.deps.config.port}`,
        LEGION_PROJECT: this.deps.state.project,
        ENVOY_NATS_URL: this.deps.config.natsUrls.join(","),
        ENVOY_URL: this.deps.config.envoyUrl,
        PATH: this.deps.panePath,
        DISPATCH_URL: this.deps.config.dispatchUrl,
        DISPATCH_TOKEN_FILE: this.dispatchTokenFile,
      });
      const window = await tmux.openWindow(this.tmux, session, "controller", [...env, shellCommand], session);
      this.deps.state.controllerLocator = {
        tmuxSession: session,
        tmuxWindowId: window.windowId,
        tmuxPaneId: window.paneId,
        socketPath,
      };
    });
    await this.persist();
  }
```

- [ ] **Step 4: `index.ts` — write `dispatch-token` at startup; prune at boot**

Import `import { DISPATCH_TOKEN_SECRET, writeSecretFile } from "./secrets";`. In
`startDaemonLocked`, directly after `resolveDaemonEnvironment` (`:353`), before the OMP probes:

```ts
  // The one Dispatch bearer every pane shares, delivered as a 0600 file pointer
  // (`DISPATCH_TOKEN_FILE`) rather than a `-e` argv value — see `secrets.ts`. An fs failure here
  // refuses startup exactly like a missing `DISPATCH_TOKEN` does: no pane may launch without it.
  if (config.dispatchToken !== undefined) {
    await writeSecretFile(config.stateDir, DISPATCH_TOKEN_SECRET, config.dispatchToken);
  }
```

After the `reconnectWorkers()` try/catch (`:422-426`):

```ts
  // Reaps pane secret files a crash left behind between clearing a locator and its save's prune.
  await processManager.pruneSecretFiles();
```

- [ ] **Step 5: `environment.ts` — strip the pointer too; rewrite the invariant comment**

`:103`: `const DISPATCH_ENV_KEYS = ["DISPATCH_TOKEN", "DISPATCH_TOKEN_FILE", "DISPATCH_URL", "DISPATCH_MCP_URL"] as const;`

Replace the doc comment `:90-102` with:

```ts
/** `DISPATCH_URL` and `DISPATCH_TOKEN_FILE` are configured pane-only exports: the only place they
 * belong is the explicit, config-driven `-e` pairs `processes.ts` adds to a spawned pane's own
 * tmux environment, and the token itself is never exported at all — panes read it from the 0600
 * file `DISPATCH_TOKEN_FILE` names (see `secrets.ts`), so no `-e` argv ever carries the bearer.
 * `DISPATCH_TOKEN` in the daemon's own environment is startup configuration only.
 * `DISPATCH_MCP_URL` is a retired alias with no legitimate destination. The tmux server that hosts
 * every Legion pane is forked by the daemon's own first `tmux -L legion-<project>` command and so
 * inherits this stripped environment — which is what makes stripping here sufficient: an `-e` pair
 * can only add or override a key for a new pane, never remove one the pane would otherwise inherit
 * from the server. Every other child process the daemon spawns (mise/tool resolution here,
 * `executePrivateKeyCommand`'s `sh -c` in `config.ts`, GitHub App role/`gh` CLI children in
 * `github-app-env.ts`, and any other daemon subprocess) must never see any of the four, even when
 * the daemon's own process (or mise's) happens to carry one for unrelated reasons. */
```

`environment.test.ts:157-206`: add `DISPATCH_TOKEN_FILE: "/leaked/dispatch-token"` to both the
`env` and the mise stdout fixtures and assert `not.toHaveProperty("DISPATCH_TOKEN_FILE")` on
`environment.paneEnv` and on `received[0]?.options?.env`.

- [ ] **Step 6: Update the existing daemon tests**

`processes.test.ts`:
- `:561-613` exact root argv: delete the pair `"-e", "LEGION_BOOT_TOKEN=boot-token"`; replace
  `"-e", "DISPATCH_TOKEN=test-dispatch-token"` with
  `"-e", \`DISPATCH_TOKEN_FILE=${path.join(stateDir, "secrets", "dispatch-token")}\``; insert
  `"-e", \`LEGION_BOOT_TOKEN_FILE=${path.join(stateDir, "secrets", roleToken("omp", root, "architect"))}\``
  immediately before the shell-command element (it is the last `-e` pair).
- `:654-682` exact environments: `LEGION_CONTROLLER_SECRET: "controller-secret"` →
  `LEGION_CONTROLLER_SECRET_FILE: path.join(stateDir, "secrets", "legion-omp-controller")`;
  `LEGION_BOOT_TOKEN: "boot-token"` →
  `LEGION_BOOT_TOKEN_FILE: path.join(stateDir, "secrets", roleToken("omp", root, "architect"))`.
  If this test's `config(stateDir)` has no `dispatchToken`, nothing else changes (no
  `DISPATCH_TOKEN_FILE` is exported).
- `:3395-3427` controller-secret rotation: in the fake `run`, on `command[3] === "new-window"`,
  read the pointer and push its contents:
  ```ts
          const pointer = tmuxWindowEnvironment(command).LEGION_CONTROLLER_SECRET_FILE;
          if (pointer) launchedSecrets.push(await readFile(pointer, "utf8"));
  ```
  and replace the two `expect(windows[n]).toContain("LEGION_CONTROLLER_SECRET=…")` lines with
  `expect(launchedSecrets).toEqual(["controller-secret-1", "controller-secret-2"]);` (this now
  proves the file held the right token *at launch time*, which the argv assertion never could).
- `:5290-5300` (worker exact env with `LEGION_BOOT_TOKEN: "worker-boot-token"`): →
  `LEGION_BOOT_TOKEN_FILE: path.join(stateDir, "secrets", roleToken("omp", <that test's issue>, <role>))`.
- `:8292-8327` and `:8329-8348`: rename to "passes DISPATCH_URL and a DISPATCH_TOKEN_FILE pointer
  to …, never the token or the retired DISPATCH_MCP_URL alias"; replace
  `expect(environment.DISPATCH_TOKEN).toBe("test-dispatch-token")` with
  `expect(environment.DISPATCH_TOKEN).toBeUndefined(); expect(environment.DISPATCH_TOKEN_FILE).toBe(path.join(stateDir, "secrets", "dispatch-token"));`.
- Any other `LEGION_BOOT_TOKEN=`/`LEGION_CONTROLLER_SECRET=`/`DISPATCH_TOKEN=` string expectation
  `rg -n '(LEGION_BOOT_TOKEN|LEGION_CONTROLLER_SECRET|DISPATCH_TOKEN)[=:]' packages/daemon/src/daemon/__tests__/processes.test.ts`
  surfaces: convert the same way.

`index.test.ts`:
- `config()` (`:217-245`): add `dispatchUrl: "http://127.0.0.1:18766", dispatchToken: "test-dispatch-token",`.
- `:160-164`: 
  ```ts
        const pointer = command.find((part) => part.startsWith("LEGION_CONTROLLER_SECRET_FILE="));
        if (pointer) {
          onControllerSecret(readFileSync(pointer.slice("LEGION_CONTROLLER_SECRET_FILE=".length), "utf8"));
        }
  ```
  (`import { readFileSync } from "node:fs";`).
- In the first test that starts a daemon and captures the controller secret, add after
  `startDaemon` resolves:
  ```ts
      const dispatchTokenFile = path.join(stateDir, "secrets", "dispatch-token");
      expect(await readFile(dispatchTokenFile, "utf8")).toBe("test-dispatch-token");
      expect((await stat(dispatchTokenFile)).mode & 0o777).toBe(0o600);
      expect((await stat(path.join(stateDir, "secrets"))).mode & 0o777).toBe(0o700);
  ```

- [ ] **Step 7: Run the daemon gates**

Run, in `packages/daemon`: `bun test`, `bunx tsc --noEmit`, `bunx biome check src/`.
Expected: all green, including both new Task 3 tests.

- [ ] **Step 8: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" commit -m "feat(daemon): pane secrets are 0600 files under <state_dir>/secrets, exported as *_FILE pointers"
```

---

### Task 4: daemon CLI reads `LEGION_CONTROLLER_SECRET_FILE`

**Files:**
- Modify: `packages/daemon/src/cli/index.ts:268-272`
- Test: `packages/daemon/src/cli/__tests__/index.test.ts`

**Interfaces — produces:**
```ts
export function resolveControllerSecret(env: NodeJS.ProcessEnv): string; // throws CliError
```

- [ ] **Step 1: Write the failing test** (append to `cli/__tests__/index.test.ts`; import
  `resolveControllerSecret` from `../index` and `mkdtemp/writeFile/rm` from `node:fs/promises`):

```ts
describe("resolveControllerSecret", () => {
  test("reads LEGION_CONTROLLER_SECRET_FILE (trimmed) ahead of LEGION_CONTROLLER_SECRET", async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), "legion-cli-secret-"));
    const file = path.join(dir, "controller");
    await writeFile(file, "  file-secret\n");
    try {
      expect(
        resolveControllerSecret({ LEGION_CONTROLLER_SECRET_FILE: file, LEGION_CONTROLLER_SECRET: "env-secret" })
      ).toBe("file-secret");
      expect(resolveControllerSecret({ LEGION_CONTROLLER_SECRET: "env-secret" })).toBe("env-secret");
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  test("fails naming the variable and path when the file is missing or empty, never falling back", async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), "legion-cli-secret-"));
    const empty = path.join(dir, "empty");
    await writeFile(empty, " \n");
    try {
      expect(() =>
        resolveControllerSecret({ LEGION_CONTROLLER_SECRET_FILE: path.join(dir, "missing"), LEGION_CONTROLLER_SECRET: "env-secret" })
      ).toThrow(`LEGION_CONTROLLER_SECRET_FILE names ${path.join(dir, "missing")}, which could not be read`);
      expect(() =>
        resolveControllerSecret({ LEGION_CONTROLLER_SECRET_FILE: empty, LEGION_CONTROLLER_SECRET: "env-secret" })
      ).toThrow(`LEGION_CONTROLLER_SECRET_FILE names ${empty}, which is empty`);
      expect(() => resolveControllerSecret({})).toThrow(
        "LEGION_CONTROLLER_SECRET or LEGION_CONTROLLER_SECRET_FILE is required for controller commands"
      );
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
});
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd -- "$LEGION_WORKSPACE/packages/daemon" && bun test src/cli/__tests__/index.test.ts`
Expected: FAIL — `resolveControllerSecret` is not exported.

- [ ] **Step 3: Implement** (replace `:268-272`; `fs` is already imported as `fs` in this file):

```ts
/** `LEGION_CONTROLLER_SECRET_FILE` (the 0600 file the daemon hands its controller pane; trimmed
 * contents) ahead of `LEGION_CONTROLLER_SECRET` (an interactive operator's own export). A set
 * pointer is authoritative: a missing, unreadable, or empty file is an error naming both the
 * variable and the path, never a fallback to the plain variable. */
export function resolveControllerSecret(env: NodeJS.ProcessEnv): string {
  const file = env.LEGION_CONTROLLER_SECRET_FILE;
  if (file !== undefined) {
    let contents: string;
    try {
      contents = fs.readFileSync(file, "utf8");
    } catch (error) {
      throw new CliError(
        `LEGION_CONTROLLER_SECRET_FILE names ${file}, which could not be read: ${error instanceof Error ? error.message : String(error)}`
      );
    }
    const secret = contents.trim();
    if (!secret) throw new CliError(`LEGION_CONTROLLER_SECRET_FILE names ${file}, which is empty`);
    return secret;
  }
  const secret = env.LEGION_CONTROLLER_SECRET;
  if (!secret) {
    throw new CliError(
      "LEGION_CONTROLLER_SECRET or LEGION_CONTROLLER_SECRET_FILE is required for controller commands"
    );
  }
  return secret;
}
```

and in `postController` (`:278`): `secret: resolveControllerSecret(process.env)`.

- [ ] **Step 4: Run to verify it passes**: same command → pass.

- [ ] **Step 5: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" commit -m "feat(cli): controller commands read LEGION_CONTROLLER_SECRET_FILE ahead of the plain variable"
```

---

### Task 5: envoy-client — `DISPATCH_TOKEN_FILE` precedence

**Files:**
- Create: `packages/envoy-client/src/secret-file.ts`, `packages/envoy-client/src/__tests__/secret-file.test.ts`
- Modify: `packages/envoy-client/src/dispatch-config.ts:81-85,95-100,132-133`
- Modify: `packages/envoy-client/package.json` (`exports["./secret-file"]`, `scripts.build` entry list)
- Modify: `packages/envoy-client/README.md:45-50`
- Test: `packages/envoy-client/src/__tests__/dispatch-config.test.ts`

**Interfaces — produces:**
```ts
// @legion/envoy-client/secret-file
export function readSecretFile(variable: string, filePath: string): string;
```
Error messages (exact, reused by Task 6 tests):
`${variable} names ${filePath}, which could not be read: ${cause}` and
`${variable} names ${filePath}, which is empty`.

- [ ] **Step 1: Write the failing tests**

```ts
// packages/envoy-client/src/__tests__/secret-file.test.ts
import { describe, expect, test } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { readSecretFile } from "../secret-file";

describe("readSecretFile", () => {
  test("returns the file contents trimmed", () => {
    const dir = mkdtempSync(path.join(os.tmpdir(), "secret-file-"));
    const file = path.join(dir, "token");
    writeFileSync(file, "  the-token\n");
    expect(readSecretFile("DISPATCH_TOKEN_FILE", file)).toBe("the-token");
  });

  test("throws naming the variable and path for an unreadable or empty file", () => {
    const dir = mkdtempSync(path.join(os.tmpdir(), "secret-file-"));
    const missing = path.join(dir, "missing");
    const empty = path.join(dir, "empty");
    writeFileSync(empty, "\n");
    expect(() => readSecretFile("DISPATCH_TOKEN_FILE", missing)).toThrow(
      `DISPATCH_TOKEN_FILE names ${missing}, which could not be read`
    );
    expect(() => readSecretFile("DISPATCH_TOKEN_FILE", empty)).toThrow(
      `DISPATCH_TOKEN_FILE names ${empty}, which is empty`
    );
  });
});
```

Append to `dispatch-config.test.ts` inside `describe("resolveDispatchConfig")`:

```ts
  test("resolves the token from DISPATCH_TOKEN_FILE ahead of DISPATCH_TOKEN and dispatch.token", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://file.test", token: "file-token" },
    });
    const tokenFile = path.join(tempDir(), "dispatch-token");
    writeFileSync(tokenFile, "pointer-token\n");

    expect(
      resolveDispatchConfig(
        {
          DISPATCH_URL: "http://override.test",
          DISPATCH_TOKEN: "environment-token",
          DISPATCH_TOKEN_FILE: tokenFile,
        },
        { home, cwd: tempDir() }
      )
    ).toEqual({ enabled: true, url: "http://override.test", token: "pointer-token", error: null });
  });

  test("disables Dispatch naming DISPATCH_TOKEN_FILE and its path when the file is unreadable or empty, without falling back", () => {
    const home = tempDir();
    writeUserConfig(home, {
      dispatch: { enabled: true, serverUrl: "http://file.test", token: "file-token" },
    });
    const missing = path.join(tempDir(), "missing");
    const unreadable = resolveDispatchConfig(
      { DISPATCH_TOKEN: "environment-token", DISPATCH_TOKEN_FILE: missing },
      { home, cwd: tempDir() }
    );
    expect(unreadable.enabled).toBe(false);
    expect(unreadable.token).toBeNull();
    expect(unreadable.error).toContain(`DISPATCH_TOKEN_FILE names ${missing}, which could not be read`);

    const empty = path.join(tempDir(), "empty");
    writeFileSync(empty, "  \n");
    const blank = resolveDispatchConfig({ DISPATCH_TOKEN_FILE: empty }, { home, cwd: tempDir() });
    expect(blank.enabled).toBe(false);
    expect(blank.token).toBeNull();
    expect(blank.error).toBe(`DISPATCH_TOKEN_FILE names ${empty}, which is empty`);
  });
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd -- "$LEGION_WORKSPACE/packages/envoy-client" && bun test`
Expected: FAIL — missing module `../secret-file`; precedence test returns `environment-token`.

- [ ] **Step 3: Implement**

```ts
// packages/envoy-client/src/secret-file.ts
import { readFileSync } from "node:fs";
import { messageFor } from "./errors";

/** Reads the secret a `<VARIABLE>_FILE` pointer names: the file's contents, trimmed. The Legion
 * daemon hands every pane secret over this way (a 0600 file under its state directory) instead of
 * as a `-e KEY=VALUE` tmux argv, which is world-readable via `/proc`. A set pointer is a claim the
 * daemon made, so a missing, unreadable, or blank file throws naming the variable and the path —
 * callers never fall back to the plain variable or a config file. */
export function readSecretFile(variable: string, filePath: string): string {
  let contents: string;
  try {
    contents = readFileSync(filePath, "utf8");
  } catch (error) {
    throw new Error(`${variable} names ${filePath}, which could not be read: ${messageFor(error)}`);
  }
  const value = contents.trim();
  if (value.length === 0) throw new Error(`${variable} names ${filePath}, which is empty`);
  return value;
}
```

`dispatch-config.ts`: import `readSecretFile` from `./secret-file`; `messageFor` is already
imported. `:81-85`:

```ts
type DispatchEnvironment = {
  readonly DISPATCH_URL?: string;
  readonly DISPATCH_TOKEN?: string;
  readonly DISPATCH_TOKEN_FILE?: string;
  readonly HOME?: string;
} & Record<string, string | undefined>;
```

Doc comment `:95-100`: "`DISPATCH_URL` overrides the file URL. The token resolves from
`DISPATCH_TOKEN_FILE` (trimmed file contents — how the Legion daemon delivers it to a pane), then
`DISPATCH_TOKEN`, then `dispatch.token`; a set `DISPATCH_TOKEN_FILE` that cannot be read or is
blank disables Dispatch with that failure as `error` and never falls back. Dispatch tools are
available only when both a URL and a bearer token resolve."

Replace `:132-133` with:

```ts
  let token: string | null;
  let tokenSource: string;
  if (env.DISPATCH_TOKEN_FILE !== undefined) {
    tokenSource = "DISPATCH_TOKEN_FILE";
    try {
      token = readSecretFile(tokenSource, env.DISPATCH_TOKEN_FILE);
    } catch (error) {
      return { enabled: false, url: url.url, token: null, error: messageFor(error) };
    }
  } else if (env.DISPATCH_TOKEN !== undefined) {
    tokenSource = "DISPATCH_TOKEN";
    token = env.DISPATCH_TOKEN;
  } else {
    tokenSource = "dispatch.token";
    token = merged.token ?? null;
  }
```

(The existing `url.error`/`url.url === null` returns at `:134-135` stay after this block; their
`token` field now carries the resolved value or `null`, as before.)

`package.json`: add `"./secret-file": { "types": "./src/secret-file.ts", "bun": "./src/secret-file.ts", "default": "./dist/secret-file.js" }`
to `exports` (alphabetical position after `./machine`) and `src/secret-file.ts` to the `build`
entry list.

`README.md:45-50`: after "override the URL and token with `DISPATCH_URL` and `DISPATCH_TOKEN`",
add: "`DISPATCH_TOKEN_FILE` (a path; the trimmed file contents are the token) wins over both — it
is how the Legion daemon hands the bearer to a pane without putting it on argv. A set
`DISPATCH_TOKEN_FILE` that is unreadable or blank disables Dispatch and names the path in
`error`; it never falls back."

- [ ] **Step 4: Run the package gates**

Run, in `packages/envoy-client`: `bun test`, `bunx tsc --noEmit`, `bunx biome check src/`,
`bun run build` (confirms the new entry builds to `dist/secret-file.js`).
Expected: green.

- [ ] **Step 5: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" commit -m "feat(envoy-client): DISPATCH_TOKEN_FILE resolves the bearer ahead of DISPATCH_TOKEN and dispatch.token"
```

---

### Task 6: pi-envoy — boot token and controller secret from `*_FILE`

**Files:**
- Modify: `packages/pi-envoy/src/legion/classify.ts:37-53`
- Modify: `packages/pi-envoy/extensions/legion.ts:177,278,368` (and import at `:7-12`)
- Test: `packages/pi-envoy/extensions/legion.test.ts` (`environmentKeys :100-113` + `originalEnvironment :114-130`, `:789-811`, new tests)
- Docs: `packages/pi-envoy/AGENTS.md:22-27`, `packages/pi-envoy/README.md:106-110`,
  `skills/legion-worker/SKILL.md:21-23`, `skills/legion-controller/SKILL.md:21-22`

**Interfaces — consumes:** `readSecretFile` from `@legion/envoy-client/secret-file` (Task 5).
**Interfaces — produces:**
```ts
export function requiredSecret(env: NodeJS.ProcessEnv, key: string): string;
```

- [ ] **Step 1: Write the failing tests in `legion.test.ts`**

Add `"LEGION_BOOT_TOKEN_FILE"`, `"LEGION_CONTROLLER_SECRET_FILE"`, `"DISPATCH_TOKEN_FILE"` to
`environmentKeys` (`:100-113`) and the matching three entries to `originalEnvironment`
(`:114-130`). Update the expected message at `:809` to:
`"LEGION_CONTROLLER_SECRET or LEGION_CONTROLLER_SECRET_FILE is required to claim the controller. Launch OMP with one of them in its environment before running /legion-claim-controller."`

New tests (place after "throws naming the missing variable when a phase worker boots without
LEGION_BOOT_TOKEN", `:907`):

```ts
  test("boots a phase worker with the boot token read from LEGION_BOOT_TOKEN_FILE, ignoring LEGION_BOOT_TOKEN", async () => {
    const requests: { readonly path: string; readonly body: unknown }[] = [];
    const workspace = await createJjWorkspace();
    const secretsDir = await mkdtemp(path.join(os.tmpdir(), "legion-secrets-"));
    temporaryPaths.push(secretsDir);
    const bootTokenFile = path.join(secretsDir, "legion-omp-repo-43-tester");
    await writeFile(bootTokenFile, "file-boot-token\n");
    process.env.LEGION_BOOT_TOKEN_FILE = bootTokenFile;

    await bootWorker({ role: "tester", workspace, requests, sessionId: "ses_file_worker" });

    const started = requests.find((request) => request.path === "/legion/v1/worker/started");
    expect((started?.body as { bootToken?: string }).bootToken).toBe("file-boot-token");
  });

  test("exits the phase worker naming LEGION_BOOT_TOKEN_FILE and its path when the file is unreadable, never falling back to LEGION_BOOT_TOKEN", async () => {
    process.env.ENVOY_URL = "http://envoy.test";
    process.env.LEGION_DAEMON_URL = "http://daemon.test";
    process.env.LEGION_TREE = "REPO-42";
    process.env.LEGION_ISSUE = "REPO-43";
    process.env.LEGION_ROLE = "tester";
    process.env.LEGION_BOOT_TOKEN = "decoy";
    process.env.LEGION_BOOT_TOKEN_FILE = "/nonexistent/legion-secrets/tester";
    globalThis.fetch = (async (_input, _init) => Response.json({})) as typeof fetch;
    const fixture = createPi();
    legionExtension(fixture.pi);
    const sessionStart = fixture.handlers.get("session_start");
    if (sessionStart === undefined) throw new Error("session_start handler was not registered");

    await expect(sessionStart({}, sessionContext("ses_bad_boot_file"))).rejects.toThrow(
      "LEGION_BOOT_TOKEN_FILE names /nonexistent/legion-secrets/tester, which could not be read"
    );
  });

  test("uses the recovery token from LEGION_BOOT_TOKEN_FILE when the daemon has lost a worker's secret", async () => {
    // Copy the body of "recovers a live worker's capability after the daemon loses its secret"
    // (:908-987), replacing `process.env.LEGION_BOOT_TOKEN = "boot-worker-recovery"` with a
    // LEGION_BOOT_TOKEN_FILE written under a temporary directory holding "boot-worker-recovery",
    // and set `process.env.LEGION_BOOT_TOKEN = "decoy"`. The existing assertion on the
    // /legion/v1/worker-session request body's `recoveryToken` === "boot-worker-recovery" stays.
  });

  test("claims the controller with the secret read from LEGION_CONTROLLER_SECRET_FILE", async () => {
    // Copy "claims the controller role at startup and on demand for an interactive session"
    // (:597-707): write "file-controller-secret" to a temp file, set
    // process.env.LEGION_CONTROLLER_SECRET_FILE to it, set LEGION_CONTROLLER_SECRET = "decoy",
    // and assert both /legion/v1/controller/ready bodies carry secret: "file-controller-secret".
  });
```

Write the two "copy" tests out in full (the plan elides them only because their fixtures are
80 lines of unchanged request routing; the changed lines are exactly the ones named).
`writeFile`/`mkdtemp` come from `node:fs/promises`, already imported in this file (check `:1-20`).

- [ ] **Step 2: Run to verify they fail**

Run: `cd -- "$LEGION_WORKSPACE/packages/pi-envoy" && bun test extensions/legion.test.ts`
Expected: the four new tests FAIL (boot token read from `LEGION_BOOT_TOKEN`, controller from
`LEGION_CONTROLLER_SECRET`); the `:789` message test FAILS on the changed wording.

- [ ] **Step 3: Implement**

`classify.ts`: add `import { readSecretFile } from "@legion/envoy-client/secret-file";` and, after
`requiredEnvironment` (`:37-41`):

```ts
/** `<key>_FILE` (trimmed file contents) ahead of `<key>`: the daemon hands every pane secret over
 * as a 0600 file pointer, never as a tmux `-e` argv value. A set pointer is authoritative — a
 * missing, unreadable, or blank file throws naming both the variable and the path, never falling
 * back to `<key>`. */
export function requiredSecret(env: NodeJS.ProcessEnv, key: string): string {
  const fileKey = `${key}_FILE`;
  const file = env[fileKey];
  if (file !== undefined) return readSecretFile(fileKey, file);
  return requiredEnvironment(env, key);
}
```

Replace `requiredControllerCapability` (`:43-53`):

```ts
export function requiredControllerCapability(env: NodeJS.ProcessEnv): string {
  if (env.LEGION_CONTROLLER_SECRET_FILE !== undefined) {
    return readSecretFile("LEGION_CONTROLLER_SECRET_FILE", env.LEGION_CONTROLLER_SECRET_FILE);
  }
  const secret = env.LEGION_CONTROLLER_SECRET;
  if (!secret) {
    throw new Error(
      "LEGION_CONTROLLER_SECRET or LEGION_CONTROLLER_SECRET_FILE is required to claim the controller. " +
        "Launch OMP with one of them in its environment before running /legion-claim-controller."
    );
  }
  return secret;
}
```

`legion.ts`: import `requiredSecret` alongside `requiredEnvironment` (`:7-12`); replace the three
`requiredEnvironment(process.env, "LEGION_BOOT_TOKEN")` (`:177`, `:278`, `:368`) with
`requiredSecret(process.env, "LEGION_BOOT_TOKEN")`. Update the comment at `:172-175`: "A worker's
boot token (read again from `LEGION_BOOT_TOKEN_FILE` here — the daemon keeps that file for as long
as the pane's locator lives) is its recovery token exactly like the root's: …".

- [ ] **Step 4: Docs**

- `packages/pi-envoy/AGENTS.md:22-27`: after "`DISPATCH_URL` and `DISPATCH_TOKEN` override those
  settings" add "; `DISPATCH_TOKEN_FILE` (a path whose trimmed contents are the token — how the
  Legion daemon delivers it to a pane) wins over both and never falls back when unreadable".
- `packages/pi-envoy/README.md:106-110`: same sentence.
- `skills/legion-worker/SKILL.md:21-23`: replace `LEGION_BOOT_TOKEN` with `LEGION_BOOT_TOKEN_FILE
  (a 0600 file under \`$LEGION_STATE_DIR/secrets\` holding your boot token; the extension reads it
  for you)`.
- `skills/legion-controller/SKILL.md:21-22`: "start OMP with `LEGION_CONTROLLER_SECRET` (or
  `LEGION_CONTROLLER_SECRET_FILE`, a path to a file holding it) and `LEGION_DAEMON_URL`".

- [ ] **Step 5: Run the package gates**

Run, in `packages/pi-envoy`: `bun test`, `bunx tsc --noEmit`, `bunx biome lint extensions/ src/`,
`bun run build` (the bundle must resolve `@legion/envoy-client/secret-file`; a failure here means
Task 5's `exports` entry is wrong).
Expected: green.

- [ ] **Step 6: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" commit -m "feat(pi-envoy): boot token and controller secret resolve from *_FILE pointers, never falling back"
```

---

### Task 7: smoke rig — private socket, checkpoint 13

**Files:**
- Modify: `scripts/smoke/checkpoints.sh` (`:76-81` helper site, `:132-162`, `:291`, `:395`, `:403`,
  `:427-429`, `:454-457`, `:490-503`, new `checkpoint_thirteen`)
- Modify: `scripts/smoke/down.sh:117-133`
- Modify: `scripts/smoke/checkpoints.test.sh:48-55` + new cases; `scripts/smoke/down.test.sh:28-31,47-55,63-71`
- Modify: `scripts/smoke/README.md:97-105,112-120,135-148`

- [ ] **Step 1: Write the failing harness cases**

`checkpoints.test.sh`: replace the fake tmux (`:48-55`) with one that understands `-L` and the
checkpoint-13 subcommands:

```bash
cat >"${fake_bin}/tmux" <<'EOF'
#!/usr/bin/env bash
socket=""
if [[ "${1:-}" == "-L" ]]; then socket="$2"; shift 2; fi
printf '%s\n' "$socket $*" >>"${TMUX_LOG:-/dev/null}"
case "${1:-}" in
  has-session) [[ -n "$socket" ]] ;;                       # default server: no legion session
  display-message) printf '%s\n' "$FAKE_TMUX_PID" ;;       # every "server pid" / "pane pid" the harness names
  show-environment) printf 'PATH=/usr/bin\n' ;;
  list-windows|list-panes)
    if [[ "$*" == *"#{window_id}"* ]]; then printf '@1\n@2\n@3\n'; else printf 'controller\nlegsmoke-1\nlegsmoke-2\n'; fi ;;
  *) printf 'controller\nlegsmoke-1\nlegsmoke-2\n' ;;
esac
EOF
```

Export `TMUX_LOG="${temporary_dir}/tmux.log"` near `CURL_LOG`. Immediately after the existing
checkpoint 1 run (before any later checkpoint appends to the log), assert every logged tmux
invocation used the socket:
`[[ -s "$TMUX_LOG" ]] && ! grep -qv '^legion-exampleorg24 ' "$TMUX_LOG" || { cat "$TMUX_LOG" >&2; exit 1; }`
(`project_slug` of `example-org/24` is `exampleorg24`).

Add two checkpoint-13 cases. The daemon state fixture at `:17` already records controller and tree
locators; append a role locator to that JSON:
`"roles":{"legion-exampleorg24-legsmoke-2-tester":{"issue":"LEGSMOKE-2","role":"tester","locator":{"tmuxWindowId":"@3","tmuxPaneId":"%4"}}}`.
The pids the fake tmux reports are two long-lived `sleep` processes the harness owns, one with a
clean environment and one with a planted `DISPATCH_TOKEN`, so `/proc/<pid>/environ` is exactly
what the checkpoint inspects (a `$PPID` trick would name a command-substitution subshell that has
already exited by the time `/proc` is read):

```bash
env -u DISPATCH_TOKEN sleep 300 &
clean_pid=$!
DISPATCH_TOKEN="leaked-into-a-pane" sleep 300 &
planted_pid=$!
trap 'kill "$clean_pid" "$planted_pid" 2>/dev/null; rm -rf "$temporary_dir"' EXIT

if ! PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" DISPATCH_URL="http://dispatch.test" FAKE_TMUX_PID="$clean_pid" \
  env -u DISPATCH_TOKEN bash "$checkpoints_script" 13 >"$output_file" 2>&1; then
  cat "$output_file" >&2; exit 1
fi
[[ "$(<"$output_file")" == *'CHECKPOINT 13 OK'* ]] || { cat "$output_file" >&2; exit 1; }
printf 'PASS: checkpoint 13 passes when no recorded process carries a secret\n'

if PATH="${fake_bin}:${PATH}" SMOKE_DIR="$smoke_dir" SMOKE_REPO="example-org/legion-smoke" \
  SMOKE_PROJECT="example-org/24" DISPATCH_URL="http://dispatch.test" FAKE_TMUX_PID="$planted_pid" \
  env -u DISPATCH_TOKEN bash "$checkpoints_script" 13 >"$output_file" 2>&1; then
  printf 'expected checkpoint 13 to fail when a recorded process environment carries DISPATCH_TOKEN\n' >&2
  exit 1
fi
[[ "$(<"$output_file")" == *"pid ${planted_pid} environ carries DISPATCH_TOKEN="* ]] || { cat "$output_file" >&2; exit 1; }
printf 'PASS: checkpoint 13 fails naming the pid and variable when a recorded process environment carries it\n'
```

(The existing `trap 'rm -rf "$temporary_dir"' EXIT` at `:13` is replaced by the one above, placed
right after the two `sleep`s start.)

`down.test.sh`: both fake tmux scripts (`:47-55`, `:63-71`) gain `if [[ "$1" == "-L" ]]; then shift 2; fi`
as their first statement, and the `:28-31` stub stays (`exit 1` for everything). Add after the
second run: assert the kill went to the private socket by logging `$*` before the shift into
`tmux_log` and checking `[[ "$(<"$tmux_log")" == *'-L legion-omp kill-session'* ]]`.

- [ ] **Step 2: Run to verify they fail**

Run: `bash scripts/smoke/checkpoints.test.sh; bash scripts/smoke/down.test.sh`
Expected: checkpoints.test.sh fails at the `TMUX_LOG` socket assertion (no `-L` yet) / usage
rejects `13`; down.test.sh fails on the `-L legion-omp kill-session` assertion.

- [ ] **Step 3: Implement `checkpoints.sh`**

After `project_slug()` (`:81`):

```bash
# Every Legion pane lives on the daemon's private tmux server, socket `legion-<slug>` (the same
# string as its session name); the default server never sees one.
legion_tmux() {
  tmux -L "legion-$(project_slug)" "$@"
}
```

Replace every `tmux ` invocation in the file (`:144`, `:152`, `:159`, `:291`, `:395`, `:403`,
`:427`, `:429`) with `legion_tmux ` (`rg -n '(^|[^_])tmux ' scripts/smoke/checkpoints.sh` must show
only the `legion_tmux()` body and the default-server probe in checkpoint 13 below).

Add `checkpoint_thirteen` after `checkpoint_twelve` (`:442`):

```bash
# Spec LEGION-6 acceptance 2 and 4: no recorded Legion process — the private tmux server itself,
# the controller pane, every tree root pane, every worker pane — carries a bearer or boot secret on
# its argv or in its environment; the private server's global environment has none; and the default
# tmux server hosts no legion-<slug> session.
checkpoint_thirteen() {
  local slug socket server_pid pane pid entry name
  local -a pids=()
  slug="$(project_slug)"
  socket="legion-${slug}"
  server_pid="$(legion_tmux display-message -p '#{pid}')" || fail "private tmux server ${socket} is not running"
  pids+=("$server_pid")
  while IFS= read -r pane; do
    [[ -n "$pane" ]] || continue
    pid="$(legion_tmux display-message -p -t "$pane" '#{pane_pid}')" || fail "recorded pane ${pane} is absent from ${socket}"
    pids+=("$pid")
  done < <(state | jq -r '
    [ .controllerLocator.tmuxPaneId?,
      (.trees[]? | .locator.tmuxPaneId?),
      (.roles[]? | select(has("issue")) | .locator.tmuxPaneId?) ]
    | map(select(. != null)) | .[]')
  ((${#pids[@]} > 1)) || fail "daemon state records no pane to inspect"
  for pid in "${pids[@]}"; do
    [[ -r "/proc/${pid}/environ" && -r "/proc/${pid}/cmdline" ]] || fail "cannot read /proc/${pid}"
    for name in DISPATCH_TOKEN LEGION_BOOT_TOKEN LEGION_CONTROLLER_SECRET; do
      if entry="$(tr '\0' '\n' <"/proc/${pid}/environ" | grep -m1 "^${name}=")"; then
        fail "pid ${pid} environ carries ${entry%%=*}=… (expected only ${name}_FILE)"
      fi
      if entry="$(tr '\0' '\n' <"/proc/${pid}/cmdline" | grep -m1 "^${name}=")"; then
        fail "pid ${pid} cmdline carries ${entry%%=*}=…"
      fi
    done
  done
  if entry="$(legion_tmux show-environment -g | grep -m1 -E '^(DISPATCH_TOKEN|LEGION_BOOT_TOKEN|LEGION_CONTROLLER_SECRET)=')"; then
    fail "private tmux server global environment carries ${entry%%=*}"
  fi
  if tmux has-session -t "$socket" 2>/dev/null; then
    fail "default tmux server still hosts a ${socket} session"
  fi
  printf 'CHECKPOINT 13 OK: %d processes on %s carry no bearer or boot secret; default server hosts no %s\n' \
    "${#pids[@]}" "$socket" "$socket"
}
```

Usage/dispatch: `:454` regex → `^([1-9]|1[0-3])$`; `:455` → `<1-13>`; add
`13) checkpoint_thirteen ;;` at `:502`. Checkpoint 13 needs no Dispatch call, so it must not call
`require_dispatch_token` (it does not).

- [ ] **Step 4: Implement `down.sh`**

`:125-132`: prefix the three `tmux` calls with `-L "$session"`:

```bash
  tmux -L "$session" has-session -t "$session" 2>/dev/null || return 0
  owner="$(tmux -L "$session" show-option -qv -t "$session" @legion_owner 2>/dev/null || true)"
  …
  tmux -L "$session" kill-session -t "$session"
  printf 'STOPPED tmux session %s on private socket %s\n' "$session" "$session"
```

- [ ] **Step 5: README**

- `:97-105`: "Tear down the processes, the private tmux server (`tmux -L legion-<slug>`), …";
  add a sentence: "Legion panes never appear in your own `tmux list-sessions`; attach with
  `tmux -L legion-<slug> attach -t legion-<slug>` where `<slug>` is `SMOKE_PROJECT` lower-cased with
  every non-alphanumeric character removed (`example-org/24` → `exampleorg24`)."
- `:113`: `<1-13>`. `:118`: "Checkpoint 13 needs no `DISPATCH_TOKEN`; run it bare."
- `:135-148` table: add
  `| 13 | — | The private tmux server and every recorded pane (controller, roots, workers) carry no `DISPATCH_TOKEN=`, `LEGION_BOOT_TOKEN=`, or `LEGION_CONTROLLER_SECRET=` on argv or in environ; the server's global environment has none; the default server hosts no `legion-<slug>` session. |`

- [ ] **Step 6: Run the harnesses**

Run: `bash scripts/smoke/checkpoints.test.sh && bash scripts/smoke/down.test.sh && bash scripts/smoke/up.test.sh`
Expected: every `PASS:` line, including the two new checkpoint-13 lines.

- [ ] **Step 7: Commit**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" commit -m "feat(smoke): private tmux socket everywhere; checkpoint 13 proves no pane or server carries a secret"
```

---

### Task 8: daemon AGENTS.md, final gates, PR

**Files:**
- Modify: `packages/daemon/src/daemon/AGENTS.md` (Files table `:35-42` region; invariants
  "Root processes and the controller are tmux windows…"; `:79-86` paragraph)

- [ ] **Step 1: AGENTS.md**

Files table: add `| \`secrets.ts\` | \`<state_dir>/secrets\` primitives: 0700 directory, 0600 files, and the prune every \`ProcessManager.persist()\` runs. |`
and extend the `tmux.ts` row: "…over an injected `run` callback, every argv prefixed
`tmux -L legion-<project>` (`TmuxServer`) — no daemon state."

Replace `:79-86` with:

```markdown
Every root, worker, and controller pane also receives `DISPATCH_URL` and `DISPATCH_TOKEN_FILE` when
`dispatch_url` is configured: `DISPATCH_URL` is the configured service base URL (no `/mcp` suffix),
and `DISPATCH_TOKEN_FILE` is `<state_dir>/secrets/dispatch-token`, a 0600 file the daemon writes at
startup from the `DISPATCH_TOKEN` environment variable (required whenever `dispatch_url` is set —
`resolveDaemonConfig` refuses to start otherwise; an fs failure writing the file refuses startup
too). Neither variable is exported when `dispatch_url` is unset; those panes fall back to their own
`envoy.json` dispatch config. The daemon never emits the retired `DISPATCH_MCP_URL` alias and strips
it — with `DISPATCH_TOKEN`, `DISPATCH_TOKEN_FILE`, and `DISPATCH_URL` — from every child process it
spawns, pane or otherwise.

No secret is ever a tmux `-e KEY=VALUE` value (a transient tmux client's argv is world-readable via
`/proc/<pid>/cmdline`). Boot tokens and the controller secret travel the same way as the Dispatch
bearer: `LEGION_BOOT_TOKEN_FILE` / `LEGION_CONTROLLER_SECRET_FILE` name a 0600 file
`<state_dir>/secrets/<role token>` (`legion-<project>-<key>-<role>`,
`legion-<project>-controller`) written immediately before that pane launches, overwritten on a
respawn of the same role, and removed by the prune `ProcessManager.persist()` runs once no live
locator references it (a boot-time prune reaps anything a crash left behind). A boot token doubles
as the pane's recovery token after a daemon restart, so its file lives exactly as long as the
pane's locator. Consumers (`@legion/envoy-client` `resolveDispatchConfig`, the pi-envoy extension,
`legion status`) resolve `X_FILE` — trimmed contents — ahead of `X`, and a set-but-unreadable or
blank file is an error naming the variable and path, never a fallback.

Every tmux command the daemon runs targets its own private server: `tmux -L legion-<project>`
(`TmuxServer` in `tmux.ts`; the socket name equals the session name). That server is forked by the
daemon's first tmux command and therefore inherits the runner's stripped `paneEnv`, never an
operator shell that may carry `DISPATCH_TOKEN`; Legion panes never appear in the operator's own
`tmux list-sessions`. Attach with `tmux -L legion-<project> attach -t legion-<project>`. A
`kill-pane` that reports `no server running` counts as the pane being gone — no server on the
daemon's own socket means no Legion pane exists. **Upgrading a live box from a default-socket
daemon:** stop the daemon, `tmux kill-session -t legion-<project>` on the default server once, start
the new daemon; `reconnectWorkers` finds every recorded socket dead and roots resurrect (`--resume`)
onto the private server. No migration code.
```

- [ ] **Step 2: Bare gates in all three packages**

Run each, in its package directory, and paste the summary lines into the PR body:
`bunx biome check src/` (daemon, envoy-client), `bunx biome lint extensions/ src/` (pi-envoy),
`bunx tsc --noEmit`, `bun test`. Then `LEGION_E2E=1 bun test src/daemon/__tests__/real-shutdown-e2e.test.ts`
in `packages/daemon`. Then `bash scripts/smoke/checkpoints.test.sh && bash scripts/smoke/down.test.sh && bash scripts/smoke/up.test.sh`.
Expected: all green.

- [ ] **Step 3: Commit, push, open the PR**

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" commit -m "docs(daemon): file-pointer secret delivery, private tmux socket, live-box upgrade step"
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" log -r 'ancestors(@, 12)'   # only LEGION-6 commits above main
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" bookmark set legion/LEGION-6 -r @- && jj -R "$LEGION_WORKSPACE" git push --bookmark legion/LEGION-6
legion gh -- pr create --repo sjawhar/legion --base main --head legion/LEGION-6 \
  --title "Dispatch token delivery: private tmux socket, secrets as 0600 files (LEGION-6)" \
  --body-file /tmp/legion-6-pr-body.md
```

PR body (READY format from `skills/legion-worker/SKILL.md`, `Dispatch: LEGION-6` line included;
CI/Threads/Thermo/E2E filled by later phases; `Fast-follow: none`; `Chain: not stacked`).

---

## Tester instructions (spec Acceptance 2–4, run on this box with the branch build)

1. Build the branch's pi-envoy plugin and point the smoke rig at it; `bash scripts/smoke/up.sh`
   per `scripts/smoke/README.md` (mode `none` suffices).
2. `secrets DISPATCH_TOKEN -- bash scripts/smoke/checkpoints.sh 1`, `… 2`, `… 3` — must pass
   unchanged (Acceptance 3: panes authenticate to Dispatch through the file).
3. `bash scripts/smoke/checkpoints.sh 13` — `CHECKPOINT 13 OK` (Acceptance 2 and 4).
4. `tmux list-sessions` (default server) → no `legion-*`; `tmux -L legion-<slug> list-sessions` →
   `legion-<slug>`. Paste both.
5. `ls -la "$SMOKE_DIR/daemon/secrets"` → directory `drwx------`, files `-rw-------` named
   `dispatch-token`, `legion-<slug>-controller`, `legion-<slug>-<key>-architect`, …
6. Negative control: plant a secret where the checkpoint looks —
   `tmux -L legion-<slug> set-environment -g LEGION_BOOT_TOKEN planted` — and re-run checkpoint
   13: it must FAIL with `private tmux server global environment carries LEGION_BOOT_TOKEN`. Then
   `tmux -L legion-<slug> set-environment -g -u LEGION_BOOT_TOKEN` and re-run → `CHECKPOINT 13 OK`.
7. `bash scripts/smoke/down.sh` → `STOPPED tmux session legion-<slug> on private socket …`;
   `tmux -L legion-<slug> list-sessions` → `no server running`.

## Observations outside this plan's scope (for the architect)

- `tmux.panePid(server, "%N")` runs `list-panes -t %N -F '#{pane_pid}'`, and tmux resolves a pane
  target to its *window* for `list-panes`, so it returns the first pane's pid in that window — for a
  split worker pane this can probe a sibling. Pre-existing, unrelated to secrets; checkpoint 13
  uses `display-message -p -t %N '#{pane_pid}'`, which targets the exact pane. Worth its own issue.

## Self-review against the spec

| spec item | task |
| :--- | :--- |
| Every tmux command carries `-L legion-<project>` | 2 |
| No bearer/boot secret as a `-e` value; panes get `*_FILE` | 3 |
| `LEGION_BOOT_TOKEN`/`LEGION_CONTROLLER_SECRET` also move to files | 3 (`launchShimmedProcess`, `spawnController`) |
| `DISPATCH_TOKEN_FILE` > `DISPATCH_TOKEN` > `dispatch.token` | 5 |
| `secrets/` 0700, files 0600, written before launch | 1, 3 |
| Per-pane file written before launch, overwritten on respawn, removed with the locator | 3 (`withPaneSecret`, `persist` prune) |
| Startup write failure refuses start | 3 Step 4 |
| Per-pane write failure = launch failure | 3 (`withPaneSecret` throws before any tmux call, inside `launchShimmedProcess`) |
| `X_FILE` bad → error naming var+path, no fallback | 4, 5, 6 |
| `X_FILE` and `X` both set → `X_FILE` wins silently | 4, 5, 6 tests |
| Post-upgrade recorded windows absent on the private socket → resurrect | 2 (gone-regex), 8 (runbook) |
| Acceptance 1 unit test | 3 Step 1 |
| Acceptance 2 checkpoint | 7 |
| Acceptance 3, 4 | tester instructions |
| Acceptance 5 gates, one PR | 8 |
| Docs: AGENTS.md, smoke README, attach command, upgrade step | 7, 8 |
| `real-shutdown-e2e.test.ts` on the private socket | 2 Step 6 |
| `environment.ts` doc comment states the file-pointer invariant | 3 Step 5 |

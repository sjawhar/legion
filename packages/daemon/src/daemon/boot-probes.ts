import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { getPluginsNodeModules } from "@oh-my-pi/pi-utils/dirs";
import type { CommandResult, CommandRunner } from "../state/fetch";
import { withOmpLaunchPrefix } from "./processes";

/** The two probes `startDaemon` starts first and awaits only at its launch hold (state load,
 * NATS, the API bind, and the worker reconnect proceed while they run; no pane opens until they
 * pass), and that `legion probe-image` runs inside the worker image
 * (packages/daemon/docker/worker.Dockerfile's last step): one module so the daemon and the image
 * gate are the same code. */
const OMP_AGENTS_CAPABILITY_MARKER = "LEGION_OMP_AGENTS=available";
const OMP_AGENTS_CAPABILITY_PROBE = `export default function probeOmpAgents(pi) {
  process.stderr.write(pi.agents ? "LEGION_OMP_AGENTS=available\\n" : "LEGION_OMP_AGENTS=missing\\n");
}
`;

/** Backoff between boot-probe attempts whose failure is transient. The delay after the i-th
 * failure is `min(initialDelayMs * 2^i, maxDelayMs)`; `maxAttempts` bounds the total number of
 * attempts, and its absence means the probe retries until it passes or fails definitively. */
export interface ProbeRetryPolicy {
  readonly initialDelayMs: number;
  readonly maxDelayMs: number;
  readonly maxAttempts?: number;
}

/** The daemon's policy: unbounded, 10 s doubling to a 5 min cap. A transient failure is OMP
 * dying under host load (its marker printed, then a non-zero exit — a contended `models.db`, a
 * starved process) or the runner killing a probe that could not finish within
 * `slow_command_timeout_seconds`; neither says anything about the configured OMP. Twice on
 * 2026-09-12 a disk storm turned one such failure into a daemon exit, and the supervisor's 1 s
 * relaunch then added an OMP spawn per second to the load it was dying of — so the daemon waits
 * the load out inside the process, however long it lasts, rather than handing the failure back
 * to the supervisor loop. A non-zero exit with no marker (the launch command failing before
 * OMP, e.g. `secrets` denying a key), the `missing` marker, or a clean exit without the marker
 * is definitive: no retry changes it, and the daemon still refuses to serve. */
export const DAEMON_PROBE_RETRY: ProbeRetryPolicy = {
  initialDelayMs: 10_000,
  maxDelayMs: 300_000,
};

/** `legion probe-image`'s policy: the same backoff, bounded to six attempts (10+20+40+80+160 s,
 * about five minutes of waiting at worst) — an image build has no supervisor and must finish. */
export const IMAGE_PROBE_RETRY: ProbeRetryPolicy = { ...DAEMON_PROBE_RETRY, maxAttempts: 6 };
/** Per-attempt budget `legion probe-image` gives each probe (the daemon's default
 * `slow_command_timeout_seconds`). */
export const IMAGE_PROBE_TIMEOUT_MS = 300_000;

export interface BootProbeOptions {
  readonly sleep: (ms: number) => Promise<void>;
  /** Per-attempt runner budget (`slow_command_timeout_seconds` in ms). */
  readonly timeoutMs: number;
  readonly retry: ProbeRetryPolicy;
}

interface ProbeOutcome {
  readonly passed: boolean;
  /** `true` when the failure is a definitive negative (retrying cannot change it). */
  readonly definitive: boolean;
  readonly detail: string;
}

/** A runner kill is transient before any marker logic: the probe never got to answer. */
function timedOutOutcome(result: CommandResult, stderrTail: string): ProbeOutcome | undefined {
  if (result.timedOut === undefined) return undefined;
  const { limitMs, elapsedMs } = result.timedOut;
  const detail = `command timed out after ${limitMs / 1000} s (ran ${(elapsedMs / 1000).toFixed(1)} s)`;
  return {
    passed: false,
    definitive: false,
    detail: stderrTail ? `${detail}\n${stderrTail}` : detail,
  };
}

/** Runs `attempt` until it passes, fails definitively, or exhausts `policy.maxAttempts`; throws
 * `makeError(detail)` in the two failing cases. Each transient failure is logged with the delay
 * before the next try, so an operator watching the supervisor log sees the daemon waiting out
 * host load instead of a silent stall. */
async function retryBootProbe(
  name: string,
  attempt: () => Promise<ProbeOutcome>,
  makeError: (detail: string) => Promise<Error>,
  policy: ProbeRetryPolicy,
  sleep: (ms: number) => Promise<void>
): Promise<void> {
  for (let i = 0; ; i++) {
    const outcome = await attempt();
    if (outcome.passed) return;
    const exhausted = policy.maxAttempts !== undefined && i + 1 >= policy.maxAttempts;
    if (outcome.definitive || exhausted) throw await makeError(outcome.detail);
    const delay = Math.min(policy.initialDelayMs * 2 ** i, policy.maxDelayMs);
    const attemptLabel =
      policy.maxAttempts === undefined ? `${i + 1}` : `${i + 1}/${policy.maxAttempts}`;
    console.error(
      `[legion] ${name} probe failed transiently (attempt ${attemptLabel}); retrying in ${delay / 1000}s${outcome.detail ? `: ${outcome.detail}` : ""}`
    );
    await sleep(delay);
  }
}

/** `exec` in the built `sh -c` command below (both this probe and `verifyLegionPluginLoaded`'s)
 * replaces the shell process image with the launch prefix/OMP invocation instead of leaving it
 * as a child: on the runner's own timeout, only the `sh` process would otherwise be killed,
 * leaving a hung prefix child (e.g. a prompting `secrets` daemon) holding the inherited pipes
 * and the daemon boot hanging. With `exec`, the kill signal reaches the real process directly. */
export async function verifyOmpAgentsCapability(
  ompInvocation: string,
  ompLaunchPrefix: readonly string[],
  runner: CommandRunner,
  options: BootProbeOptions
): Promise<void> {
  const probeDir = await mkdtemp(path.join(os.tmpdir(), "legion-omp-probe-"));
  const probePath = path.join(probeDir, "probe.mjs");
  try {
    await writeFile(probePath, OMP_AGENTS_CAPABILITY_PROBE, "utf8");
    await retryBootProbe(
      "OMP pi.agents",
      async () => {
        const result = await runner(
          [
            "sh",
            "-c",
            `exec ${withOmpLaunchPrefix(ompLaunchPrefix, ompInvocation)} models --no-extensions --extension "$1" --json >/dev/null`,
            "sh",
            probePath,
          ],
          { timeoutMs: options.timeoutMs }
        );
        const output = `${result.stderr}\n${result.stdout}`;
        const detail = [result.stderr.trim(), result.stdout.trim()].filter(Boolean).join("\n");
        const timedOut = timedOutOutcome(result, detail);
        if (timedOut) return timedOut;
        if (result.exitCode === 0 && output.includes(OMP_AGENTS_CAPABILITY_MARKER)) {
          return { passed: true, definitive: false, detail };
        }
        // Transient only when OMP got as far as loading the probe extension (marker present) and
        // then died. Everything else is an answer no retry changes: a clean exit without the
        // marker, the extension reporting `missing`, or the launch command failing before OMP.
        const transient =
          result.exitCode !== 0 &&
          output.includes(OMP_AGENTS_CAPABILITY_MARKER) &&
          !output.includes("LEGION_OMP_AGENTS=missing");
        return { passed: false, definitive: !transient, detail };
      },
      async (detail) =>
        new Error(
          `[legion] Configured OMP invocation does not expose pi.agents${detail ? `: ${detail}` : ""}`
        ),
      options.retry,
      options.sleep
    );
  } finally {
    await rm(probeDir, { recursive: true, force: true });
  }
}

// Read by legion.ts (packages/pi-envoy/extensions/legion.ts) on load: proves the
// extension actually loaded through OMP's own extension pipeline, not merely that
// its manifest file exists on disk. A manifest-only check would pass even when the
// plugin is disabled (`omp plugin disable`) or unregistered, in which case OMP's
// ambient discovery silently skips it and every spawned session is Legion-less.
const LEGION_LOADED_MARKER = "LEGION_PLUGIN_LOADED=yes";
/** Caps how much of a failed launch probe's stderr lands in the thrown error message — a
 * misbehaving launch prefix (e.g. a wrapper that dumps a stack trace) must not blow up the
 * daemon's own startup-failure log line; the tail is kept since that's where the actual error
 * usually is. */
const MAX_PROBE_STDERR_LENGTH = 2048;
const LEGION_LOAD_PROBE = `export default function probeLegionPluginLoaded(pi) {
  const loaded = globalThis[Symbol.for("legion.pi-envoy.legion-loaded")];
  process.stderr.write(loaded ? "LEGION_PLUGIN_LOADED=yes\\n" : "LEGION_PLUGIN_LOADED=no\\n");
}
`;

// A daemon and the OMP sessions it spawns share one ambient environment (Legion
// never sets `--profile`/`OMP_PROFILE` for spawned sessions), so this probe's
// invocation — no `--extension` beyond the probe's own — matches the daemon's real
// spawn shape closely enough that ambient discovery resolves the same plugin root
// a spawned session will load from.
//
// Known gap: this probe runs from the daemon's own cwd, not a spawned root's
// `workspace.workspaceDir`. A target repo that commits `.omp/plugin-overrides.json`
// disabling `pi-legion-envoy` passes this boot gate but still launches a
// Legion-less session. That is caught at runtime instead: such a session never
// calls `/process/started` or `/worker/started`, and the boot handshake treats an
// unclaimed boot token as a launch failure (see T5/T9).
export async function verifyLegionPluginLoaded(
  ompInvocation: string,
  ompLaunchPrefix: readonly string[],
  runner: CommandRunner,
  readPluginManifest: (manifestPath: string) => Promise<string>,
  options: BootProbeOptions
): Promise<void> {
  const probeDir = await mkdtemp(path.join(os.tmpdir(), "legion-plugin-probe-"));
  const probePath = path.join(probeDir, "probe.mjs");
  const launchCommand = withOmpLaunchPrefix(ompLaunchPrefix, ompInvocation);
  try {
    await writeFile(probePath, LEGION_LOAD_PROBE, "utf8");
    let lastExitCode = 0;
    await retryBootProbe(
      "pi-legion-envoy load",
      async () => {
        const result = await runner(
          [
            "sh",
            "-c",
            `exec ${launchCommand} models --extension "$1" --json >/dev/null`,
            "sh",
            probePath,
          ],
          { timeoutMs: options.timeoutMs }
        );
        lastExitCode = result.exitCode;
        const stderrTail = result.stderr.trim().slice(-MAX_PROBE_STDERR_LENGTH);
        const timedOut = timedOutOutcome(result, stderrTail);
        if (timedOut) return timedOut;
        const output = `${result.stderr}\n${result.stdout}`;
        if (result.exitCode === 0 && output.includes(LEGION_LOADED_MARKER)) {
          return { passed: true, definitive: false, detail: "" };
        }
        // Transient only when omp loaded the plugin (marker present) and then died under load. A
        // non-zero exit without the marker is the launch command (the configured
        // `omp_launch_prefix` plus the OMP invocation) failing before or inside omp — e.g.
        // `secrets` denying a key — a definitive launch failure with its own message below; the
        // plugin-disabled diagnosis would send the operator to `omp plugin list` when the fix is
        // the prefix/credential.
        const transient = result.exitCode !== 0 && output.includes(LEGION_LOADED_MARKER);
        return { passed: false, definitive: !transient, detail: stderrTail };
      },
      async (detail) => {
        if (lastExitCode !== 0) {
          return new Error(
            `[legion] OMP launch probe failed (exit ${lastExitCode}) for launch command "${launchCommand}"${detail ? `: ${detail}` : ""}`
          );
        }
        // exit 0, marker simply absent: the plugin is genuinely disabled or unregistered. The
        // manifest read is a best-effort version hint for this message only — never part of the
        // pass/fail gate, so a passing boot reads no manifest.
        const pluginVersion = await readPluginManifest(
          path.join(getPluginsNodeModules(), "@sjawhar", "pi-legion-envoy", "package.json")
        )
          .then((raw) => {
            const manifest: { readonly version?: string } = JSON.parse(raw);
            return manifest.version;
          })
          .catch(() => undefined);
        return new Error(
          `[legion] pi-legion-envoy${pluginVersion ? ` ${pluginVersion}` : ""} is installed but not loaded by omp (disabled or unregistered); run omp plugin list`
        );
      },
      options.retry,
      options.sleep
    );
  } finally {
    await rm(probeDir, { recursive: true, force: true });
  }
}

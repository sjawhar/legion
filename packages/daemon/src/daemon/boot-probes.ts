import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { LEGION_DAEMON_API_VERSION } from "@legion/contracts";
import { getPluginsNodeModules } from "@oh-my-pi/pi-utils/dirs";
import type { CommandResult, CommandRunner } from "../state/fetch";
import { DEFAULT_SLOW_COMMAND_TIMEOUT_SECONDS } from "./config";
import { withOmpLaunchPrefix } from "./processes";

/** The two probes `startDaemon` starts first and awaits only at its launch hold (state load,
 * NATS, the API bind, and the worker reconnect proceed while they run; no pane opens until they
 * pass), and that `legion probe-image` runs inside the worker image
 * (packages/daemon/docker/worker.Dockerfile's last step): one module so the daemon and the image
 * gate are the same code. */
const OMP_AGENTS_CAPABILITY_MARKER = "LEGION_OMP_AGENTS=available";
const OMP_AGENTS_MISSING_MARKER = "LEGION_OMP_AGENTS=missing";
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
/** Per-attempt budget `legion probe-image` gives each probe: the daemon's default
 * `slow_command_timeout_seconds`, so the image gate and a default-configured daemon agree. */
export const IMAGE_PROBE_TIMEOUT_MS = DEFAULT_SLOW_COMMAND_TIMEOUT_SECONDS * 1000;

export interface BootProbeOptions {
  readonly sleep: (ms: number) => Promise<void>;
  /** Per-attempt runner budget (`slow_command_timeout_seconds` in ms). */
  readonly timeoutMs: number;
  readonly retry: ProbeRetryPolicy;
  /** Aborts the probe: a daemon whose boot failed for another reason (state load, NATS, the
   * API bind) must neither spawn another OMP after its pending backoff nor leave an attempt's
   * OMP child running behind it. Passed to every runner call (which kills the child on abort)
   * and checked before every attempt, after every failed attempt, and after every sleep; the
   * probe then rejects with `ProbeAbortedError`. */
  readonly signal?: AbortSignal;
}

/** The probe chain was cancelled by the daemon's own teardown — while an attempt was running
 * (the runner killed it on the signal) or while it was waiting to retry. */
class ProbeAbortedError extends Error {
  constructor(name: string) {
    super(
      `[legion] ${name} probe abandoned: the daemon stopped while it was running or waiting to retry`
    );
    this.name = "ProbeAbortedError";
  }
}

interface ProbeOutcome {
  readonly passed: boolean;
  /** `true` when the failure is a definitive negative (retrying cannot change it). */
  readonly definitive: boolean;
  /** `true` when the runner killed the attempt because the caller's signal aborted: the daemon
   * gave the probe up, so the attempt is neither a failure to log nor an answer to diagnose. */
  readonly aborted?: boolean;
  readonly detail: string;
}

/** Classifies a kill the runner made. A kill on the caller's abort is not the probe's failure at
 * all — the caller is tearing down and its own error is what surfaces — so it is reported as
 * `aborted` before anything else is read. A budget kill is transient when the probe never got to
 * answer — but a probe that printed its negative marker and only then hung past the budget has
 * answered: that answer is definitive, so `negativeMarker` is classified first and the kill is
 * reported only for a marker-less output. */
function killedOutcome(
  result: CommandResult,
  stderrTail: string,
  negativeMarker: string
): ProbeOutcome | undefined {
  if (result.aborted) return { passed: false, definitive: false, aborted: true, detail: "" };
  if (result.timedOut === undefined) return undefined;
  if (`${result.stderr}\n${result.stdout}`.includes(negativeMarker)) return undefined;
  const { limitMs, elapsedMs } = result.timedOut;
  const detail = `command timed out after ${limitMs / 1000} s (ran ${(elapsedMs / 1000).toFixed(1)} s)`;
  return {
    passed: false,
    definitive: false,
    detail: stderrTail ? `${detail}\n${stderrTail}` : detail,
  };
}

/** Runs `attempt` until it passes, fails definitively, or exhausts `policy.maxAttempts`; throws
 * `makeError(detail, reason)` in the two failing cases — `"definitive"` for an answer no retry
 * changes, `"exhausted"` when a bounded policy ran out of attempts while still transient, so the
 * message can say the probe never completed rather than misreport what it never answered. Each
 * transient failure is logged with the delay before the next try, so an operator watching the
 * supervisor log sees the daemon waiting out host load instead of a silent stall. An aborted
 * `signal` ends the loop with `ProbeAbortedError` — before an attempt, after one the runner killed
 * on the abort, or after a failed one that finished just as the daemon gave up — with no
 * transient log and no retry announced: a line promising a retry that will not happen would
 * mislead the operator reading the start-up error that follows it. An attempt that *passed* as
 * the signal fired still returns as passed: nothing about the probe's answer changed, and the
 * two-probe chain stops at the next probe's loop-top check before anything is spawned. */
async function retryBootProbe(
  name: string,
  attempt: () => Promise<ProbeOutcome>,
  makeError: (detail: string, reason: "definitive" | "exhausted") => Promise<Error>,
  policy: ProbeRetryPolicy,
  sleep: (ms: number) => Promise<void>,
  signal: AbortSignal | undefined
): Promise<void> {
  for (let i = 0; ; i++) {
    if (signal?.aborted) throw new ProbeAbortedError(name);
    const outcome = await attempt();
    if (outcome.passed) return;
    if (outcome.aborted || signal?.aborted) throw new ProbeAbortedError(name);
    if (outcome.definitive) throw await makeError(outcome.detail, "definitive");
    if (policy.maxAttempts !== undefined && i + 1 >= policy.maxAttempts) {
      throw await makeError(outcome.detail, "exhausted");
    }
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
          { timeoutMs: options.timeoutMs, signal: options.signal }
        );
        const output = `${result.stderr}\n${result.stdout}`;
        const detail = [result.stderr.trim(), result.stdout.trim()].filter(Boolean).join("\n");
        const killed = killedOutcome(result, detail, OMP_AGENTS_MISSING_MARKER);
        if (killed) return killed;
        if (result.exitCode === 0 && output.includes(OMP_AGENTS_CAPABILITY_MARKER)) {
          return { passed: true, definitive: false, detail };
        }
        // Transient only when OMP got as far as loading the probe extension (marker present) and
        // then died. Everything else is an answer no retry changes: a clean exit without the
        // marker, the extension reporting `missing`, or the launch command failing before OMP.
        const transient =
          result.exitCode !== 0 &&
          output.includes(OMP_AGENTS_CAPABILITY_MARKER) &&
          !output.includes(OMP_AGENTS_MISSING_MARKER);
        return { passed: false, definitive: !transient, detail };
      },
      async (detail, reason) =>
        new Error(
          reason === "exhausted"
            ? `[legion] OMP pi.agents probe never completed within its retry budget (${options.retry.maxAttempts} attempts)${detail ? `: ${detail}` : ""}`
            : `[legion] Configured OMP invocation does not expose pi.agents${detail ? `: ${detail}` : ""}`
        ),
      options.retry,
      options.sleep,
      options.signal
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
const LEGION_NOT_LOADED_MARKER = "LEGION_PLUGIN_LOADED=no";
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

/** The installed `@sjawhar/pi-legion-envoy` manifest, wherever OMP's ambient discovery resolves
 * the plugin root (`getPluginsNodeModules`: the active profile, then the default roots). */
function legionPluginManifestPath(): string {
  return path.join(getPluginsNodeModules(), "@sjawhar", "pi-legion-envoy", "package.json");
}

/**
 * Refuses startup unless the installed plugin was built against this daemon's contract: its
 * manifest's `legion.daemonApiVersion` must equal `LEGION_DAEMON_API_VERSION`
 * (`@legion/contracts`). The number covers two surfaces — the `LegionDaemonApi` HTTP request and
 * response shapes (2 was introduced by LEGION-20 for `stateGate`/`GatesRegister`), and the pane
 * contract (every environment variable the daemon sets on a pane that the plugin reads or
 * writes — the full list, the bump rule, and the contract history live in the constant's doc
 * comment; covered by 2 from LEGION-52). A plugin from before an HTTP shape change (or after a
 * later one) validates every daemon response against the strict schemas it bundles and fails the
 * controller/architect boot handshake — `daemon.state()` rejects on the first unknown field —
 * with nothing in the daemon's own logs to say why; a plugin from before a pane contract change
 * never writes the credential file the daemon names, and every worker fails at its first
 * `legion gh`/`jj git push` after the work is done, with `grantFrom`'s `LEGION_GRANT_FILE names
 * <path>, which could not be read: ENOENT …: the pi-envoy extension in this pane did not write it
 * — the installed plugin predates LEGION-54` (cli/index.ts). This makes either skew a loud boot
 * failure instead. Read on every boot: a missing or unreadable manifest, or one without the
 * field, is a refusal, never a fallback (the load probe below would report such a plugin as
 * merely "not loaded", sending the operator to `omp plugin list` when the fix is a reinstall).
 */
export async function verifyLegionPluginContract(
  readPluginManifest: (manifestPath: string) => Promise<string>
): Promise<void> {
  const manifestPath = legionPluginManifestPath();
  const refuse = (packageVersion: string, contractVersion: string): Error =>
    new Error(
      `[legion] pi-legion-envoy at ${manifestPath} (package ${packageVersion}) speaks daemon API contract ${contractVersion}; this daemon requires ${LEGION_DAEMON_API_VERSION}. Install the @sjawhar/pi-legion-envoy release built from this daemon's commit into the active profile.`
    );
  let manifest: unknown;
  try {
    manifest = JSON.parse(await readPluginManifest(manifestPath));
  } catch (error) {
    throw new Error(
      `[legion] pi-legion-envoy manifest at ${manifestPath} could not be read (${error instanceof Error ? error.message : String(error)}); this daemon requires a plugin speaking daemon API contract ${LEGION_DAEMON_API_VERSION}. Install the @sjawhar/pi-legion-envoy release built from this daemon's commit into the active profile.`
    );
  }
  const record = typeof manifest === "object" && manifest !== null ? manifest : {};
  const packageVersion =
    "version" in record && typeof record.version === "string" ? record.version : "unknown";
  const legion = "legion" in record ? record.legion : undefined;
  const contractVersion =
    typeof legion === "object" && legion !== null && "daemonApiVersion" in legion
      ? legion.daemonApiVersion
      : undefined;
  if (contractVersion !== LEGION_DAEMON_API_VERSION) {
    throw refuse(
      packageVersion,
      contractVersion === undefined ? "none" : JSON.stringify(contractVersion)
    );
  }
}

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
    let answeredNotLoaded = false;
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
          { timeoutMs: options.timeoutMs, signal: options.signal }
        );
        lastExitCode = result.exitCode;
        const stderrTail = result.stderr.trim().slice(-MAX_PROBE_STDERR_LENGTH);
        const killed = killedOutcome(result, stderrTail, LEGION_NOT_LOADED_MARKER);
        if (killed) return killed;
        const output = `${result.stderr}\n${result.stdout}`;
        if (result.exitCode === 0 && output.includes(LEGION_LOADED_MARKER)) {
          return { passed: true, definitive: false, detail: "" };
        }
        // The probe answered "not loaded": that diagnosis stands whatever the exit code — a
        // runner kill after the marker still exits non-zero, and must not read as a launch failure.
        answeredNotLoaded = output.includes(LEGION_NOT_LOADED_MARKER);
        // Transient only when omp loaded the plugin (marker present) and then died under load. A
        // non-zero exit without the marker is the launch command (the configured
        // `omp_launch_prefix` plus the OMP invocation) failing before or inside omp — e.g.
        // `secrets` denying a key — a definitive launch failure with its own message below; the
        // plugin-disabled diagnosis would send the operator to `omp plugin list` when the fix is
        // the prefix/credential.
        const transient = result.exitCode !== 0 && output.includes(LEGION_LOADED_MARKER);
        return { passed: false, definitive: !transient, detail: stderrTail };
      },
      async (detail, reason) => {
        if (reason === "exhausted") {
          return new Error(
            `[legion] pi-legion-envoy load probe never completed within its retry budget (${options.retry.maxAttempts} attempts) for launch command "${launchCommand}"${detail ? `: ${detail}` : ""}`
          );
        }
        if (lastExitCode !== 0 && !answeredNotLoaded) {
          return new Error(
            `[legion] OMP launch probe failed (exit ${lastExitCode}) for launch command "${launchCommand}"${detail ? `: ${detail}` : ""}`
          );
        }
        // exit 0, marker simply absent: the plugin is genuinely disabled or unregistered. The
        // manifest read here is a best-effort version hint for this message only; the contract
        // gate (`verifyLegionPluginContract`) already read and validated it before this probe.
        const pluginVersion = await readPluginManifest(legionPluginManifestPath())
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
      options.sleep,
      options.signal
    );
  } finally {
    await rm(probeDir, { recursive: true, force: true });
  }
}

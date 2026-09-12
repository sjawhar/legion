import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { LEGION_DAEMON_API_VERSION } from "@legion/contracts";
import { getPluginsNodeModules } from "@oh-my-pi/pi-utils/dirs";
import type { CommandRunner } from "../state/fetch";
import { withOmpLaunchPrefix } from "./processes";

/** The two probes `startDaemon` runs before it owns state or serves the API, and that
 * `legion probe-image` runs inside the worker image (packages/daemon/docker/worker.Dockerfile's last
 * step): one module so the daemon and the image gate are the same code. */
const OMP_AGENTS_CAPABILITY_MARKER = "LEGION_OMP_AGENTS=available";
const OMP_AGENTS_CAPABILITY_PROBE = `export default function probeOmpAgents(pi) {
  process.stderr.write(pi.agents ? "LEGION_OMP_AGENTS=available\\n" : "LEGION_OMP_AGENTS=missing\\n");
}
`;

/** Backoff between boot-probe attempts whose failure is transient: OMP reached the probe
 * extension (its marker is in the output) and then exited non-zero — it died under host load (a
 * contended `models.db`, a starved process), not because of what the probe asks. A non-zero exit
 * with no marker is the launch command failing before OMP (e.g. `secrets` denying a key) and stays
 * a definitive failure, as does a clean exit whose answer is negative. Twice on 2026-09-12 a disk storm turned one such
 * exit into a daemon exit, and the supervisor's 1 s relaunch then added an OMP spawn per second
 * to the load it was dying of. Bounded: after the last attempt the failure is fatal as before. */
const PROBE_RETRY_DELAYS_MS: readonly number[] = [5_000, 15_000, 45_000, 90_000, 180_000];

interface ProbeOutcome {
  readonly passed: boolean;
  /** `true` when the failure is a definitive negative (retrying cannot change it). */
  readonly definitive: boolean;
  readonly detail: string;
}

/** Runs `attempt` until it passes, fails definitively, or exhausts `PROBE_RETRY_DELAYS_MS`;
 * throws `makeError(detail)` in the two failing cases. Each transient failure is logged with the
 * delay before the next try, so an operator watching the supervisor log sees the daemon waiting
 * out host load instead of a silent stall. */
async function retryBootProbe(
  name: string,
  attempt: () => Promise<ProbeOutcome>,
  makeError: (detail: string) => Promise<Error>,
  sleep: (ms: number) => Promise<void>
): Promise<void> {
  for (let i = 0; ; i++) {
    const outcome = await attempt();
    if (outcome.passed) return;
    const delay = PROBE_RETRY_DELAYS_MS[i];
    if (outcome.definitive || delay === undefined) throw await makeError(outcome.detail);
    console.error(
      `[legion] ${name} probe failed transiently (attempt ${i + 1}/${PROBE_RETRY_DELAYS_MS.length + 1}); retrying in ${delay / 1000}s${outcome.detail ? `: ${outcome.detail}` : ""}`
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
  sleep: (ms: number) => Promise<void>
): Promise<void> {
  const probeDir = await mkdtemp(path.join(os.tmpdir(), "legion-omp-probe-"));
  const probePath = path.join(probeDir, "probe.mjs");
  try {
    await writeFile(probePath, OMP_AGENTS_CAPABILITY_PROBE, "utf8");
    await retryBootProbe(
      "OMP pi.agents",
      async () => {
        const result = await runner([
          "sh",
          "-c",
          `exec ${withOmpLaunchPrefix(ompLaunchPrefix, ompInvocation)} models --no-extensions --extension "$1" --json >/dev/null`,
          "sh",
          probePath,
        ]);
        const output = `${result.stderr}\n${result.stdout}`;
        const detail = [result.stderr.trim(), result.stdout.trim()].filter(Boolean).join("\n");
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
      sleep
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

/** The installed `@sjawhar/pi-legion-envoy` manifest, wherever OMP's ambient discovery resolves
 * the plugin root (`getPluginsNodeModules`: the active profile, then the default roots). */
function legionPluginManifestPath(): string {
  return path.join(getPluginsNodeModules(), "@sjawhar", "pi-legion-envoy", "package.json");
}

/**
 * Refuses startup unless the installed plugin was built against this daemon's HTTP API contract:
 * its manifest's `legion.daemonApiVersion` must equal `LEGION_DAEMON_API_VERSION`
 * (`@legion/contracts`). The plugin validates every daemon response against the strict schemas
 * it bundles, so a plugin from before a shape change (or after a later one) fails the
 * controller/architect boot handshake — `daemon.state()` rejects on the first unknown field —
 * with nothing in the daemon's own logs to say why; this makes the skew a loud boot failure
 * instead. Read on every boot: a missing or unreadable manifest, or one without the field, is a
 * refusal, never a fallback (the load probe below would report such a plugin as merely "not
 * loaded", sending the operator to `omp plugin list` when the fix is a reinstall).
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
  sleep: (ms: number) => Promise<void>
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
        const result = await runner([
          "sh",
          "-c",
          `exec ${launchCommand} models --extension "$1" --json >/dev/null`,
          "sh",
          probePath,
        ]);
        lastExitCode = result.exitCode;
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
        return {
          passed: false,
          definitive: !transient,
          detail: result.stderr.trim().slice(-MAX_PROBE_STDERR_LENGTH),
        };
      },
      async (detail) => {
        if (lastExitCode !== 0) {
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
      sleep
    );
  } finally {
    await rm(probeDir, { recursive: true, force: true });
  }
}

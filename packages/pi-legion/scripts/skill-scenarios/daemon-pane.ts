/**
 * The rig's reading of the Legion daemon: `daemon-pane.go`, run with `go run -overlay` as a main
 * inside a checkout's daemon module, which is how it reaches the daemon's internal packages. The
 * rig asks it rather than restating the daemon: a change to the daemon reaches the rig by
 * construction. `workerPane` builds a phase worker's whole pane environment over it.
 */
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import type { LegionRole } from "@legion/contracts";
import { DEFAULT_ENVOY_URL } from "@legion/envoy-client/envoy-settings";
import { sessionConfigFile, sessionEnvoySettings } from "@legion/envoy-client/session-settings";

/** This checkout's daemon module, the one the rig reads unless a run names another checkout's. */
export const DAEMON_MODULE = path.resolve(import.meta.dir, "../../../daemon");

const PROGRAM = path.join(import.meta.dir, "daemon-pane.go");

/** Runs daemon-pane.go in `module` with `args`, `input` on its stdin (the caller's own stdin when
 * there is none), and returns its stdout. */
export function runDaemonPane(module: string, args: readonly string[], input?: string): string {
  const scratch = mkdtempSync(path.join(os.tmpdir(), "daemon-pane-"));
  try {
    const overlay = path.join(scratch, "overlay.json");
    const main = path.join(module, "cmd", "daemon-pane", "main.go");
    writeFileSync(overlay, JSON.stringify({ Replace: { [main]: PROGRAM } }));
    const result = Bun.spawnSync(["go", "run", "-overlay", overlay, "./cmd/daemon-pane", ...args], {
      cwd: module,
      stdin: input === undefined ? "inherit" : new TextEncoder().encode(input),
      stdout: "pipe",
      stderr: "pipe",
    });
    if (result.exitCode !== 0) {
      throw new Error(
        `daemon-pane ${args.join(" ")} in ${module} exited ${result.exitCode}: ${result.stderr.toString().trim()}`
      );
    }
    return result.stdout.toString();
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

/** The claim a pane is launched for and the rig's stand-ins for what the daemon knows at boot
 * (daemon-pane.go's paneRequest). `ghToken` stands in for the claim's GitHub App token: it is
 * rendered into the claim's gh files under `<stateDir>/secrets/<claim>-gh`, the pane's
 * `GH_CONFIG_DIR`; omitted, the pane gets no gh variable, as from a daemon with no GitHub Apps. */
interface PaneRequest {
  readonly project: string;
  readonly issue: string;
  readonly role: string;
  readonly stateDir: string;
  readonly workspace: string;
  readonly daemonUrl: string;
  readonly envoyUrl: string;
  readonly natsUrls: readonly string[];
  readonly bootTokenFile: string;
  readonly ghToken?: string;
  readonly path: string;
  readonly systemPrompt?: boolean;
}

/** What the daemon tells the pane: every variable, PATH included, and, for a request that asks for
 * it, the one `--append-system-prompt` argument as shell text. */
export interface DaemonPane {
  readonly env: Record<string, string>;
  readonly systemPromptArgument?: string;
}

/** The stand-in for the claim's GitHub App token: `daemon-pane.go` renders it into the claim's gh
 * files, and the pane's own `gh auth token` prints it. A placeholder, never a secret. */
export const RIG_GH_TOKEN = "ghs_rig_token";

/** Where a worker pane runs: the run directory that stands for the daemon's state directory and
 * the issue's workspace, the stand-in daemon's port, and the Oh My Pi profile. */
export interface WorkerLaunch {
  readonly rig: string;
  readonly port: number;
  readonly profile: string;
}

/** The claim a worker pane is launched for: its project, issue and role. */
export interface WorkerClaim {
  readonly project: string;
  readonly issue: string;
  readonly role: LegionRole;
}

/** Where a pane's daemon is read from: a checkout's daemon module (`DAEMON_MODULE` for this one),
 * whose embedded role prompts compose the pane's system prompt when `systemPrompt` is set. */
export interface PaneSource {
  readonly daemonModule: string;
  readonly systemPrompt?: boolean;
}

/** The pane the Legion daemon's tmux runtime gives a phase worker for `claim`, pointed at the
 * scratch state directory and the stand-in daemon, over the caller's own environment. Everything
 * the daemon tells the pane comes from the daemon's own functions in `source`'s module
 * (`daemon-pane.go`): the claim's identity, the daemon URL, the state directory and workspace,
 * Envoy (the listener and NATS the pane's own extension reaches from the caller's environment and
 * the profile's envoy.json, `envoySettings`, which reads no Envoy from the workspace's
 * `.envoy/envoy.json`; the default listener and no NATS when that Envoy is off), `GH_CONFIG_DIR`
 * naming the claim's gh files, which hold `RIG_GH_TOKEN`, with `GH_TOKEN`, `GITHUB_TOKEN` and
 * `GH_HOST` set empty,
 * `PI_SHELL_PREFIX`, the four XDG base directories, the boot token pointer
 * (`<state>/secrets/boot`, the file the stand-in checks), and PATH with the launcher directory
 * first; no grant file, since the `legion` tool mints its grants in-process and no agent runs
 * `legion` from bash; and, with `systemPrompt`, the system prompt argument. An inherited
 * `ANTHROPIC_API_KEY`, every `LEGION_*` and `DISPATCH_*` value and the caller's own GitHub
 * variables are dropped first. The profile's agent directory, and so its envoy.json, is under the
 * inherited `HOME`. Throws `envoy: <why>` when that Envoy is off with an error, or on with inbound
 * messages off (`inbound.off`: no NATS server, or its seed refused), since the pane's Legion claim
 * refuses then. */
export function workerPane(
  launch: WorkerLaunch,
  inherited: NodeJS.ProcessEnv,
  claim: WorkerClaim,
  source: PaneSource
): DaemonPane {
  const env: Record<string, string> = {};
  for (const [key, value] of Object.entries(inherited)) {
    if (value === undefined) continue;
    if (key === "ANTHROPIC_API_KEY" || key.startsWith("LEGION_") || key.startsWith("DISPATCH_"))
      continue;
    if (["GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN", "GH_HOST"].includes(key)) continue;
    if (key === "OMP_SESSION_ID" || key === "TMUX" || key === "TMUX_PANE") continue;
    env[key] = value;
  }
  const home = env.HOME ?? os.homedir();
  const stateDir = path.join(launch.rig, "state");
  const workspace = path.join(launch.rig, "ws");
  const profileEnv = {
    ...env,
    OMP_PROFILE: launch.profile,
    PI_CODING_AGENT_DIR: path.join(home, ".omp", "profiles", launch.profile, "agent"),
  };
  const envoy = sessionEnvoySettings(sessionConfigFile("omp", profileEnv), profileEnv, {
    cwd: workspace,
  });
  if (!envoy.enabled && envoy.error !== null) throw new Error(`envoy: ${envoy.error}`);
  if (envoy.enabled && "off" in envoy.inbound) throw new Error(`envoy: ${envoy.inbound.off}`);
  const request: PaneRequest = {
    ...claim,
    stateDir,
    workspace,
    daemonUrl: `http://127.0.0.1:${launch.port}`,
    envoyUrl: envoy.enabled ? envoy.url : DEFAULT_ENVOY_URL,
    natsUrls: envoy.enabled && "natsUrls" in envoy.inbound ? envoy.inbound.natsUrls : [],
    bootTokenFile: path.join(stateDir, "secrets", "boot"),
    ghToken: RIG_GH_TOKEN,
    path: env.PATH ?? "",
    systemPrompt: source.systemPrompt,
  };
  const pane = JSON.parse(
    runDaemonPane(source.daemonModule, ["pane"], JSON.stringify(request))
  ) as DaemonPane;
  return {
    ...pane,
    env: {
      ...env,
      OMP_PROFILE: profileEnv.OMP_PROFILE,
      PI_PROFILE: launch.profile,
      PI_CODING_AGENT_DIR: profileEnv.PI_CODING_AGENT_DIR,
      PI_NOTIFICATIONS: "off",
      PI_NO_TITLE: "1",
      ...pane.env,
    },
  };
}

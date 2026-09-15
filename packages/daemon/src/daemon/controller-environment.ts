import path from "node:path";
import { grantSecretName, secretFilePath } from "./secrets";
import { workerBinDir } from "./worker-bin";

/** The credential environment a root, worker, or controller process carries for life — never per
 * command. `LEGION_GRANT_FILE` names the 0600 file under `<state_dir>/secrets` the pi-envoy
 * extension writes each bash command's freshly minted grant to (and `legion credential`,
 * `legion gh`, and `legion handoff complete` read ahead of `LEGION_GRANT`); the daemon only names
 * it here and prunes it with the pane's boot-token file (`trackProcessSecrets`). PATH puts
 * `<state_dir>/worker-bin` (the `gh` shim `index.ts` installs at startup) first exactly once:
 * `processPath` is the daemon's resolved PATH with every inherited `worker-bin` entry already
 * stripped by `resolveDaemonEnvironment` (a daemon started from inside a Legion pane inherits
 * that pane's shim-first PATH through `mise env`), so the daemon's own `gh` is never the shim and
 * the prefix added here is the only one. How PATH reaches the process is the runtime's business:
 * this record is runtime-neutral, and the tmux runtime delivers PATH through the pane's shell
 * command rather than as a `-e` pair, since tmux replaces a pane's PATH from the unattached
 * client's environment after copying the `-e` pairs (LEGION-91) — a Kubernetes runtime sets it on
 * the pod verbatim, and `legion controller start` on the operator's own process.
 * `GH_CONFIG_DIR` isolates a raw `gh` from any operator login state, and the emptied
 * `GH_TOKEN`/`GITHUB_TOKEN`/`GH_HOST` (tmux renders `''` as `-e KEY=`, an empty variable) keep an
 * ambient token or host from shadowing the per-call one `legion gh` redeems. */
export function credentialProcessEnvironment(
  stateDir: string,
  processPath: string,
  token: string
): Record<string, string> {
  return {
    PATH: `${workerBinDir(stateDir)}${path.delimiter}${processPath}`,
    GH_CONFIG_DIR: path.join(stateDir, "gh"),
    GH_TOKEN: "",
    GITHUB_TOKEN: "",
    GH_HOST: "",
    LEGION_GRANT_FILE: secretFilePath(stateDir, grantSecretName(token)),
  };
}

export interface ControllerEnvironmentInput {
  project: string;
  /** `controllerToken(project)`: names the grant file. */
  token: string;
  daemonUrl: string;
  stateDir: string;
  /** The PATH the credential environment is prepended to: the daemon's `processPath`
   * (`<state_dir>/bin` first), or the CLI's launcher-first PATH. */
  processPath: string;
  natsUrls: readonly string[];
  envoyUrl: string;
  dispatchUrl: string | undefined;
  dispatchTokenFile: string | undefined;
}

/** The controller's environment, in the exact key order `processes.test.ts` pins for the pane's
 * `-e` pairs: `ProcessManager.spawnController` (tmux) and `legion controller start` (the operator's
 * machine) both build it here, so the two can never drift (LEGION-25 Part B). Secrets are not
 * here: they travel as `<NAME>_FILE` pointers the caller adds (`LEGION_CONTROLLER_SECRET_FILE`,
 * `ENVOY_TOKEN_FILE`). */
export function controllerProcessEnvironment(
  input: ControllerEnvironmentInput
): Record<string, string | undefined> {
  return {
    LEGION_CONTROLLER: "1",
    LEGION_ROLE: "controller",
    LEGION_DAEMON_URL: input.daemonUrl,
    LEGION_PROJECT: input.project,
    // Every daemon-launched pane carries its state directory (`legion state` reads it); the
    // credential keys below are what `legion gh --` needs.
    LEGION_STATE_DIR: input.stateDir,
    ENVOY_NATS_URL: input.natsUrls.join(","),
    ENVOY_URL: input.envoyUrl,
    ...credentialProcessEnvironment(input.stateDir, input.processPath, input.token),
    DISPATCH_URL: input.dispatchUrl,
    DISPATCH_TOKEN_FILE: input.dispatchTokenFile,
  };
}

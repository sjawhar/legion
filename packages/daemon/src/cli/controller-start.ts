import fs from "node:fs";
import path from "node:path";
import { controllerToken, LegionDaemonApi } from "@legion/contracts";
import { parse } from "yaml";
import {
  legionProjectToken,
  normalizeBaseUrl,
  readArgv,
  readString,
  readStringArray,
  requireNoMcpSuffix,
  requireNonEmpty,
  validateUrl,
} from "../daemon/config";
import { controllerProcessEnvironment } from "../daemon/controller-environment";
import {
  materializeDeploymentInstructions,
  readDeploymentInstructions,
} from "../daemon/deployment-instructions";
import { installLegionCliLauncher, resolveRolePromptsDir } from "../daemon/environment";
import { DEFAULT_OMP_INVOCATION } from "../daemon/omp-pin";
import { resolveLegionPaths } from "../daemon/paths";
import { systemPromptArguments, withOmpLaunchPrefix } from "../daemon/runtime-tmux";
import { writeSecretFile } from "../daemon/secrets";
import { installWorkerGhShim, pathWithoutWorkerBin } from "../daemon/worker-bin";
import { CliError } from "./errors";
import { readSecretPointer } from "./secret-pointer";

/** The example the refusal messages point at; shipped beside the manifests. */
export const CONTROLLER_CONFIG_EXAMPLE = "deploy/kubernetes/daemon/controller.yaml.example";

/** Every key the operator-side file may carry — the same names as `legion.yaml`, only the ones the
 * controller needs. */
export const CONTROLLER_CONFIG_KEYS = [
  "project",
  "daemon_url",
  "operator_token_file",
  "envoy_url",
  "envoy_token_file",
  "nats_urls",
  "dispatch_url",
  "dispatch_token_file",
  "instructions",
  "omp_invocation",
  "omp_launch_prefix",
  "state_dir",
] as const;

export interface ControllerStartConfig {
  project: string;
  daemonUrl: string;
  /** Absolute. */
  operatorTokenFile: string;
  envoyUrl: string;
  /** Absolute. */
  envoyTokenFile?: string;
  natsUrls: string[];
  dispatchUrl?: string;
  /** Absolute. */
  dispatchTokenFile?: string;
  /** Absolute. */
  instructions?: string;
  /** Default `DEFAULT_OMP_INVOCATION`. */
  ompInvocation: string;
  /** Default `[]`. */
  ompLaunchPrefix: string[];
  /** Absolute when set. */
  stateDir?: string;
}

/** Strict: a YAML mapping whose every key is one of `CONTROLLER_CONFIG_KEYS` — a typo fails here,
 * on the laptop, not in the pod. Never the daemon's loader: that one runs `private_key_command`
 * and demands the image, namespace, and Envoy token the controller never uses. Relative paths
 * resolve against `configDir`; `~` is not expanded (the daemon loader's rule). */
export function loadControllerStartConfig(
  yamlText: string,
  configDir: string
): ControllerStartConfig {
  let parsed: unknown;
  try {
    parsed = parse(yamlText);
  } catch (error) {
    throw new CliError(
      `Invalid controller configuration: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new CliError("controller.yaml must be a mapping");
  }
  const config = parsed as Record<string, unknown>;
  const known: ReadonlySet<string> = new Set(CONTROLLER_CONFIG_KEYS);
  const unknown = Object.keys(config).filter((key) => !known.has(key));
  if (unknown.length > 0) {
    throw new CliError(
      unknown
        .map(
          (key) =>
            `unknown key "${key}" in the controller configuration; legion controller start reads only ${CONTROLLER_CONFIG_KEYS.join(", ")} — see ${CONTROLLER_CONFIG_EXAMPLE}`
        )
        .join("; ")
    );
  }
  const resolvePath = (value: string): string =>
    path.isAbsolute(value) ? value : path.resolve(configDir, value);
  const asCli = <T>(read: () => T): T => {
    try {
      return read();
    } catch (error) {
      if (error instanceof CliError) throw error;
      throw new CliError(error instanceof Error ? error.message : String(error));
    }
  };
  const required = (key: (typeof CONTROLLER_CONFIG_KEYS)[number]): string => {
    const value = asCli(() => readString(config[key], key));
    if (value === undefined)
      throw new CliError(`${key} is required in the controller configuration`);
    return asCli(() => requireNonEmpty(value, key));
  };
  const optionalPath = (key: (typeof CONTROLLER_CONFIG_KEYS)[number]): string | undefined => {
    const value = asCli(() => readString(config[key], key));
    return value === undefined ? undefined : resolvePath(asCli(() => requireNonEmpty(value, key)));
  };

  // Sanitized exactly as the daemon sanitizes its own `project` (`legionProjectToken`), so the
  // value an operator copies from the cluster's legion.yaml — `sjawhar/legion` — names the same
  // controller token, secret file, and LEGION_PROJECT the daemon derived: `sjawharlegion`.
  const project = asCli(() => legionProjectToken(required("project"), "project"));
  const daemonUrl = asCli(() => validateUrl(required("daemon_url"), "daemon_url"));
  const operatorTokenFile = resolvePath(required("operator_token_file"));
  const envoyUrl = asCli(() => validateUrl(required("envoy_url"), "envoy_url"));
  const natsUrls = asCli(() => readStringArray(config.nats_urls, "nats_urls"));
  if (natsUrls === undefined || natsUrls.length === 0) {
    throw new CliError("nats_urls is required in the controller configuration");
  }
  const envoyTokenFile = optionalPath("envoy_token_file");
  const rawDispatchUrl = asCli(() => readString(config.dispatch_url, "dispatch_url"));
  const dispatchTokenFile = optionalPath("dispatch_token_file");
  if (rawDispatchUrl !== undefined && dispatchTokenFile === undefined) {
    throw new CliError("dispatch_url is set but dispatch_token_file is not");
  }
  if (rawDispatchUrl === undefined && dispatchTokenFile !== undefined) {
    throw new CliError("dispatch_token_file is set but dispatch_url is not");
  }
  const dispatchUrl =
    rawDispatchUrl === undefined
      ? undefined
      : asCli(() =>
          normalizeBaseUrl(
            requireNoMcpSuffix(validateUrl(rawDispatchUrl, "dispatch_url"), "dispatch_url"),
            "dispatch_url"
          )
        );
  const instructions = optionalPath("instructions");
  const ompInvocation = asCli(() => readString(config.omp_invocation, "omp_invocation"));
  const ompLaunchPrefix = asCli(() => readArgv(config.omp_launch_prefix, "omp_launch_prefix"));
  const stateDir = optionalPath("state_dir");

  return {
    project,
    daemonUrl,
    operatorTokenFile,
    envoyUrl,
    ...(envoyTokenFile === undefined ? {} : { envoyTokenFile }),
    natsUrls,
    ...(dispatchUrl === undefined ? {} : { dispatchUrl }),
    ...(dispatchTokenFile === undefined ? {} : { dispatchTokenFile }),
    ...(instructions === undefined ? {} : { instructions }),
    ompInvocation:
      ompInvocation === undefined
        ? DEFAULT_OMP_INVOCATION
        : asCli(() => requireNonEmpty(ompInvocation, "omp_invocation")),
    ompLaunchPrefix: ompLaunchPrefix ?? [],
    ...(stateDir === undefined ? {} : { stateDir }),
  };
}

/** The operator token: a regular file readable by its owner only (`mode & 0o077 === 0`), trimmed
 * non-empty contents. Refused before anything is fetched or written — the token is the one thing
 * that buys a controller secret, and a group- or world-readable copy is a second way in. */
export function readOperatorTokenFile(file: string): string {
  let stats: fs.Stats;
  try {
    stats = fs.statSync(file);
  } catch (error) {
    throw new CliError(
      `operator_token_file names ${file}, which could not be read: ${(error as Error).message}`
    );
  }
  if (!stats.isFile()) {
    throw new CliError(`operator_token_file names ${file}, which is not a regular file`);
  }
  const mode = stats.mode & 0o777;
  if ((mode & 0o077) !== 0) {
    throw new CliError(
      `operator_token_file ${file} is readable by its group or others (mode 0${mode.toString(8)}); chmod 0600 it`
    );
  }
  return readSecretPointer("operator_token_file", file);
}

/** The one HTTP call the command makes; `typeof fetch` would also demand Bun's `preconnect`. */
export type SecretFetch = (input: string, init?: RequestInit) => Promise<Response>;

export interface ControllerStartDeps {
  env: NodeJS.ProcessEnv;
  fetch: SecretFetch;
  homeDir: string;
  /** Runs the controller in the foreground (`sh -c <command>`, stdio inherited) and resolves to
   * its exit code. */
  spawn(launch: { command: string; cwd: string; env: NodeJS.ProcessEnv }): Promise<number>;
  log(line: string): void;
}

/** `POST /legion/v1/controller/secret` with the operator token as a bearer. A rejected fetch names
 * the daemon URL and never tries another address; a non-2xx quotes the daemon's `error`. */
async function fetchControllerSecret(
  fetch: SecretFetch,
  daemonUrl: string,
  operatorToken: string
): Promise<string> {
  const url = `${daemonUrl}/legion/v1/controller/secret`;
  let response: Response;
  try {
    response = await fetch(url, {
      method: "POST",
      headers: { authorization: `Bearer ${operatorToken}`, "content-type": "application/json" },
      body: "{}",
    });
  } catch (error) {
    throw new CliError(
      `could not reach the Legion daemon at ${daemonUrl}: ${error instanceof Error ? error.message : String(error)}; is the port-forward running? (never falls back to another address)`
    );
  }
  const text = await response.text();
  if (!response.ok) {
    let detail = text;
    try {
      const body: unknown = JSON.parse(text);
      if (body !== null && typeof body === "object" && "error" in body) {
        detail = String(body.error);
      }
    } catch {
      // A non-JSON body is quoted as text.
    }
    throw new CliError(
      `${url} answered ${response.status}: ${detail}${response.status === 403 ? " — the operator token does not match the daemon's operator_token_file, or this daemon has none configured" : ""}`
    );
  }
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch {
    throw new CliError(`${url} answered with a body that is not JSON`);
  }
  const parsed = LegionDaemonApi.ControllerSecret.response.safeParse(body);
  if (!parsed.success) {
    throw new CliError(`${url} answered with an unexpected body: ${parsed.error.message}`);
  }
  return parsed.data.secret;
}

/**
 * `legion controller start --config <controller.yaml> [--daemon-url <url>]`: the operator's side
 * of the in-cluster controller (LEGION-25 Part B). In order, and nothing is written or launched
 * until the daemon has answered: read the strict operator-side file; take `--daemon-url` over its
 * `daemon_url`; refuse an operator token file others can read, and a blank or unreadable Envoy or
 * Dispatch token file (the plugin reads those itself later — refusing early is the point); fetch
 * the controller secret with the operator token as a bearer (the daemon mints it exactly as it
 * does for its own pane: the previous controller's secret stops working); write it 0600 under the
 * local state directory beside the `gh` shim, the `legion` launcher, and the deployment
 * instructions a pane gets; then run the same interactive OMP command the tmux daemon runs —
 * `omp_launch_prefix` + `omp_invocation`, one joined `--append-system-prompt`, no `--resume`, no
 * `--mode rpc` — in the foreground with the shared controller environment, and exit with its code.
 */
export async function cmdControllerStart(
  options: { configPath: string; daemonUrl?: string },
  deps: ControllerStartDeps
): Promise<number> {
  let absolute: string;
  try {
    absolute = fs.realpathSync(options.configPath);
  } catch (error) {
    throw new CliError(
      `controller configuration ${options.configPath} could not be read: ${(error as Error).message}`
    );
  }
  const config = loadControllerStartConfig(
    fs.readFileSync(absolute, "utf8"),
    path.dirname(absolute)
  );
  let daemonUrl: string;
  try {
    daemonUrl =
      options.daemonUrl === undefined
        ? config.daemonUrl
        : validateUrl(options.daemonUrl, "--daemon-url");
  } catch (error) {
    throw new CliError(error instanceof Error ? error.message : String(error));
  }
  daemonUrl = daemonUrl.replace(/\/+$/, "");

  const operatorToken = readOperatorTokenFile(config.operatorTokenFile);
  // Every value derived from the file is fixed before the daemon is asked: the token below is the
  // secret file's name and the grant file's, and `config.project` is already the daemon's token.
  const token = controllerToken(config.project);
  // Everything below up to the fetch is a local check that reads and writes nothing under the
  // state directory: the daemon mints a fresh controller secret on every request and revokes the
  // incumbent controller's, so a failure this machine can detect on its own must be found first
  // — never after the running controller has been cut off for nothing.
  // The token files: only to refuse an unreadable or blank one; the plugin reads them itself.
  if (config.envoyTokenFile !== undefined) {
    readSecretPointer("envoy_token_file", config.envoyTokenFile);
  }
  if (config.dispatchTokenFile !== undefined) {
    readSecretPointer("dispatch_token_file", config.dispatchTokenFile);
  }
  // The role prompts: `resolveRolePromptsDir` stats every `ROLE_PROMPT_FILES` entry —
  // `controller-root.md` among them — exactly as the daemon does at boot.
  let rolePromptsDir: string;
  try {
    rolePromptsDir = resolveRolePromptsDir(deps.env);
  } catch (error) {
    throw new CliError(error instanceof Error ? error.message : String(error));
  }
  const promptPath = path.join(rolePromptsDir, "controller-root.md");
  // The instructions file: read now, written under the state directory only after the fetch.
  if (config.instructions !== undefined) {
    try {
      await readDeploymentInstructions(config.instructions);
    } catch (error) {
      throw new CliError(error instanceof Error ? error.message : String(error));
    }
  }

  const secret = await fetchControllerSecret(deps.fetch, daemonUrl, operatorToken);

  const stateDir =
    config.stateDir ??
    path.join(resolveLegionPaths(deps.env, deps.homeDir).stateDir, `${config.project}-controller`);
  const secretFile = await writeSecretFile(stateDir, token, secret);
  await installWorkerGhShim(stateDir);
  const binDir = await installLegionCliLauncher(stateDir);
  let instructionsFile: string | undefined;
  if (config.instructions !== undefined) {
    try {
      instructionsFile = await materializeDeploymentInstructions(
        config.instructions,
        stateDir,
        config.project
      );
    } catch (error) {
      throw new CliError(error instanceof Error ? error.message : String(error));
    }
  }
  const controllerDir = path.join(stateDir, "controller");
  fs.mkdirSync(controllerDir, { recursive: true });

  const processPath = `${binDir}${path.delimiter}${pathWithoutWorkerBin(deps.env.PATH ?? "")}`;
  const env: NodeJS.ProcessEnv = { ...deps.env };
  const controllerEnv = controllerProcessEnvironment({
    project: config.project,
    token,
    daemonUrl,
    stateDir,
    processPath,
    natsUrls: config.natsUrls,
    envoyUrl: config.envoyUrl,
    dispatchUrl: config.dispatchUrl,
    dispatchTokenFile: config.dispatchTokenFile,
  });
  for (const [key, value] of Object.entries(controllerEnv)) {
    if (value !== undefined) env[key] = value;
  }
  env.LEGION_CONTROLLER_SECRET_FILE = secretFile;
  if (config.envoyTokenFile !== undefined) env.ENVOY_TOKEN_FILE = config.envoyTokenFile;

  const command = `${withOmpLaunchPrefix(config.ompLaunchPrefix, config.ompInvocation)} ${systemPromptArguments(promptPath, undefined, instructionsFile)}`;
  deps.log(
    `[legion] starting the controller for ${config.project} against ${daemonUrl}; state in ${stateDir}`
  );
  return await deps.spawn({ command, cwd: controllerDir, env });
}

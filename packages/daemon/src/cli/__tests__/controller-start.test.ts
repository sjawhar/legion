import { afterAll, describe, expect, it } from "bun:test";
import { existsSync, realpathSync } from "node:fs";
import { chmod, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { SOURCE_ROLE_PROMPTS_DIR } from "../../daemon/environment";
import { systemPromptArguments, withOmpLaunchPrefix } from "../../daemon/runtime-tmux";
import {
  CONTROLLER_CONFIG_EXAMPLE,
  type ControllerStartDeps,
  cmdControllerStart,
  loadControllerStartConfig,
} from "../controller-start";
import { CliError } from "../errors";

const tempDirs: string[] = [];

/** Canonical (realpath) so expectations match the command's `realpathSync` of the config file. */
async function tempDir(prefix: string): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), prefix));
  tempDirs.push(dir);
  return realpathSync(dir);
}

afterAll(async () => {
  await Promise.all(tempDirs.map((dir) => rm(dir, { recursive: true, force: true })));
});

const OPERATOR_TOKEN = "op-tok-7f3a";
const DAEMON_URL = "http://daemon.test:13370";

interface FixtureOptions {
  /** Extra or replacement lines for `controller.yaml`; the defaults name every required key. */
  yaml?: string[];
  /** Replaces the default file wholesale. */
  yamlText?: string;
  /** The operator token file's mode; the default is what the command demands. */
  tokenMode?: number;
  tokenContents?: string;
  /** Leave out the token file altogether. */
  omitToken?: boolean;
  omitEnvoyToken?: boolean;
  omitDispatch?: boolean;
  /** The daemon's answer to `POST /controller/secret`. */
  secretResponse?: () => Response | Promise<Response>;
  /** `fetch` rejects with this error instead of answering. */
  fetchError?: Error;
  /** Oh My Pi's exit code. */
  exitCode?: number;
  env?: NodeJS.ProcessEnv;
}

interface RecordedFetch {
  url: string;
  method: string | undefined;
  authorization: string | null;
  contentType: string | null;
  body: string | undefined;
}

interface RecordedSpawn {
  command: string;
  cwd: string;
  env: NodeJS.ProcessEnv;
}

async function fixture(options: FixtureOptions = {}) {
  const dir = await tempDir("legion-controller-start-");
  const homeDir = await tempDir("legion-controller-home-");
  const tokenFile = path.join(dir, "operator-token");
  if (!options.omitToken) {
    await writeFile(tokenFile, options.tokenContents ?? `${OPERATOR_TOKEN}\n`, {
      mode: options.tokenMode ?? 0o600,
    });
    await chmod(tokenFile, options.tokenMode ?? 0o600);
  }
  await writeFile(path.join(dir, "instructions.md"), "Always be kind.\n", "utf8");
  if (!options.omitEnvoyToken) {
    await writeFile(path.join(dir, "envoy-token"), "envoy-bearer\n", { mode: 0o600 });
  }
  if (!options.omitDispatch) {
    await writeFile(path.join(dir, "dispatch-token"), "dispatch-bearer\n", { mode: 0o600 });
  }
  const lines = options.yaml ?? [
    "project: demo",
    `daemon_url: ${DAEMON_URL}`,
    "operator_token_file: ./operator-token",
    "envoy_url: http://envoy.test:9020",
    ...(options.omitEnvoyToken ? [] : ["envoy_token_file: envoy-token"]),
    "nats_urls: [nats://a:4222, nats://b:4222]",
    ...(options.omitDispatch
      ? []
      : ["dispatch_url: https://dispatch.test", "dispatch_token_file: ./dispatch-token"]),
    "instructions: ./instructions.md",
    "omp_invocation: mise x github:sjawhar/oh-my-pi@1 -- omp",
    "omp_launch_prefix: [secrets, ANTHROPIC_API_KEY, --]",
  ];
  const configPath = path.join(dir, "controller.yaml");
  await writeFile(configPath, options.yamlText ?? `${lines.join("\n")}\n`, "utf8");

  const fetches: RecordedFetch[] = [];
  const spawns: RecordedSpawn[] = [];
  const logs: string[] = [];
  const env: NodeJS.ProcessEnv = options.env ?? {
    PATH: "/usr/bin:/opt/x/worker-bin",
    HOME: homeDir,
    TERM: "xterm",
  };
  const deps: ControllerStartDeps = {
    env,
    homeDir,
    fetch: async (input, init) => {
      const headers = new Headers(init?.headers);
      fetches.push({
        url: String(input),
        method: init?.method,
        authorization: headers.get("authorization"),
        contentType: headers.get("content-type"),
        body: typeof init?.body === "string" ? init.body : undefined,
      });
      if (options.fetchError) throw options.fetchError;
      return options.secretResponse
        ? await options.secretResponse()
        : Response.json({ secret: "s3cret" });
    },
    spawn: async (launch) => {
      spawns.push(launch);
      return options.exitCode ?? 0;
    },
    log: (line) => logs.push(line),
  };
  return {
    dir,
    homeDir,
    configPath,
    tokenFile,
    deps,
    fetches,
    spawns,
    logs,
    env,
    stateDir: path.join(homeDir, ".local", "state", "legion", "demo-controller"),
  };
}

async function modeOf(file: string): Promise<number> {
  return (await stat(file)).mode & 0o777;
}

describe("legion controller start", () => {
  it("fetches the controller secret with the operator token as a bearer and an empty JSON body", async () => {
    const f = await fixture();
    await cmdControllerStart({ configPath: f.configPath }, f.deps);
    expect(f.fetches).toEqual([
      {
        url: `${DAEMON_URL}/legion/v1/controller/secret`,
        method: "POST",
        authorization: `Bearer ${OPERATOR_TOKEN}`,
        contentType: "application/json",
        body: "{}",
      },
    ]);
  });

  it("writes the secret 0600, the gh shim 0700, the legion launcher, and the deployment instructions under the state directory", async () => {
    const f = await fixture();
    await cmdControllerStart({ configPath: f.configPath }, f.deps);
    const secretFile = path.join(f.stateDir, "secrets", "legion-demo-controller");
    expect(await readFile(secretFile, "utf8")).toBe("s3cret");
    expect(await modeOf(secretFile)).toBe(0o600);
    expect(await modeOf(path.join(f.stateDir, "secrets"))).toBe(0o700);
    expect(await modeOf(path.join(f.stateDir, "worker-bin", "gh"))).toBe(0o700);
    expect((await stat(path.join(f.stateDir, "bin", "legion"))).mode & 0o111).not.toBe(0);
    expect(await readFile(path.join(f.stateDir, "deployment-instructions.md"), "utf8")).toStartWith(
      "# Deployment instructions (demo)\n\nAlways be kind."
    );
    expect(f.spawns[0]?.cwd).toBe(path.join(f.stateDir, "controller"));
    expect((await stat(path.join(f.stateDir, "controller"))).isDirectory()).toBe(true);
  });

  it("launches Oh My Pi in the foreground with the shared controller environment and one joined system prompt", async () => {
    const f = await fixture();
    await cmdControllerStart({ configPath: f.configPath }, f.deps);
    expect(f.spawns).toHaveLength(1);
    const launch = f.spawns[0];
    if (!launch) throw new Error("no spawn");
    expect(launch.command).toBe(
      `${withOmpLaunchPrefix(["secrets", "ANTHROPIC_API_KEY", "--"], "mise x github:sjawhar/oh-my-pi@1 -- omp")} ${systemPromptArguments(
        path.join(SOURCE_ROLE_PROMPTS_DIR, "controller-root.md"),
        undefined,
        path.join(f.stateDir, "deployment-instructions.md")
      )}`
    );
    expect(launch.command).not.toContain("--resume");
    expect(launch.command).not.toContain("--mode rpc");
    const added = Object.fromEntries(
      Object.entries(launch.env).filter(([key, value]) => f.env[key] !== value)
    );
    expect(added).toEqual({
      LEGION_CONTROLLER: "1",
      LEGION_ROLE: "controller",
      LEGION_DAEMON_URL: DAEMON_URL,
      LEGION_PROJECT: "demo",
      LEGION_STATE_DIR: f.stateDir,
      ENVOY_NATS_URL: "nats://a:4222,nats://b:4222",
      ENVOY_URL: "http://envoy.test:9020",
      PATH: `${path.join(f.stateDir, "worker-bin")}:${path.join(f.stateDir, "bin")}:/usr/bin`,
      GH_CONFIG_DIR: path.join(f.stateDir, "gh"),
      GH_TOKEN: "",
      GITHUB_TOKEN: "",
      GH_HOST: "",
      LEGION_GRANT_FILE: path.join(f.stateDir, "secrets", "legion-demo-controller-grant"),
      DISPATCH_URL: "https://dispatch.test",
      DISPATCH_TOKEN_FILE: path.join(f.dir, "dispatch-token"),
      LEGION_CONTROLLER_SECRET_FILE: path.join(f.stateDir, "secrets", "legion-demo-controller"),
      ENVOY_TOKEN_FILE: path.join(f.dir, "envoy-token"),
    });
    // Inherited values survive; no secret value ever rides the environment.
    expect(launch.env.HOME).toBe(f.homeDir);
    expect(launch.env.TERM).toBe("xterm");
    for (const key of ["LEGION_CONTROLLER_SECRET", "ENVOY_TOKEN", "DISPATCH_TOKEN"]) {
      expect(launch.env).not.toHaveProperty(key);
    }
    expect(JSON.stringify(launch)).not.toContain("s3cret");
    expect(f.logs).toEqual([
      `[legion] starting the controller for demo against ${DAEMON_URL}; state in ${f.stateDir}`,
    ]);
  });

  it("omits the optional Dispatch and Envoy-token pointers when the file has none", async () => {
    const f = await fixture({ omitDispatch: true, omitEnvoyToken: true });
    await cmdControllerStart({ configPath: f.configPath }, f.deps);
    const env = f.spawns[0]?.env ?? {};
    for (const key of ["DISPATCH_URL", "DISPATCH_TOKEN_FILE", "ENVOY_TOKEN_FILE"]) {
      expect(env).not.toHaveProperty(key);
    }
    expect(env.LEGION_CONTROLLER).toBe("1");
  });

  it("exits with Oh My Pi's exit code", async () => {
    const f = await fixture({ exitCode: 3 });
    expect(await cmdControllerStart({ configPath: f.configPath }, f.deps)).toBe(3);
  });

  it("--daemon-url overrides the file for both the secret request and LEGION_DAEMON_URL", async () => {
    const f = await fixture();
    await cmdControllerStart(
      { configPath: f.configPath, daemonUrl: "http://127.0.0.1:13370/" },
      f.deps
    );
    expect(f.fetches[0]?.url).toBe("http://127.0.0.1:13370/legion/v1/controller/secret");
    expect(f.spawns[0]?.env.LEGION_DAEMON_URL).toBe("http://127.0.0.1:13370");
  });

  it("a 403 from the daemon exits naming the URL and mints nothing locally", async () => {
    const f = await fixture({
      secretResponse: () => Response.json({ error: "Invalid operator token" }, { status: 403 }),
    });
    const failure = await cmdControllerStart({ configPath: f.configPath }, f.deps).catch(
      (error) => error
    );
    expect(failure).toBeInstanceOf(CliError);
    expect((failure as CliError).message).toBe(
      `${DAEMON_URL}/legion/v1/controller/secret answered 403: Invalid operator token — the operator token does not match the daemon's operator_token_file, or this daemon has none configured`
    );
    expect(existsSync(path.join(f.stateDir, "secrets"))).toBe(false);
    expect(f.spawns).toEqual([]);
  });

  it("an unreachable daemon exits naming the URL and never tries another address", async () => {
    const f = await fixture({ fetchError: new Error("ECONNREFUSED") });
    const failure = await cmdControllerStart({ configPath: f.configPath }, f.deps).catch(
      (error) => error
    );
    expect(failure).toBeInstanceOf(CliError);
    expect((failure as CliError).message).toBe(
      `could not reach the Legion daemon at ${DAEMON_URL}: ECONNREFUSED; is the port-forward running? (never falls back to another address)`
    );
    expect(f.fetches).toHaveLength(1);
    expect(existsSync(f.stateDir)).toBe(false);
    expect(f.spawns).toEqual([]);
  });

  it("refuses a group- or world-readable operator token file naming the path and mode", async () => {
    const f = await fixture({ tokenMode: 0o640 });
    const failure = await cmdControllerStart({ configPath: f.configPath }, f.deps).catch(
      (error) => error
    );
    expect(failure).toBeInstanceOf(CliError);
    expect((failure as CliError).message).toBe(
      `operator_token_file ${f.tokenFile} is readable by its group or others (mode 0640); chmod 0600 it`
    );
    expect(f.fetches).toEqual([]);
  });

  it("refuses a missing or blank operator token file naming the path", async () => {
    const missing = await fixture({ omitToken: true });
    const missingFailure = await cmdControllerStart(
      { configPath: missing.configPath },
      missing.deps
    ).catch((error) => error);
    expect(missingFailure).toBeInstanceOf(CliError);
    expect((missingFailure as CliError).message).toStartWith(
      `operator_token_file names ${missing.tokenFile}, which could not be read: ENOENT`
    );
    expect(missing.fetches).toEqual([]);

    const blank = await fixture({ tokenContents: " \n" });
    const blankFailure = await cmdControllerStart(
      { configPath: blank.configPath },
      blank.deps
    ).catch((error) => error);
    expect((blankFailure as CliError).message).toBe(
      `operator_token_file names ${blank.tokenFile}, which is empty`
    );
    expect(blank.fetches).toEqual([]);
  });

  it("refuses an unknown key naming it and the example file", async () => {
    const f = await fixture({
      yaml: [
        "project: demo",
        `daemon_url: ${DAEMON_URL}`,
        "operator_token_file: ./operator-token",
        "envoy_url: http://envoy.test:9020",
        "nats_urls: [nats://a:4222]",
        "runtime: kubernetes",
      ],
    });
    const failure = await cmdControllerStart({ configPath: f.configPath }, f.deps).catch(
      (error) => error
    );
    expect(failure).toBeInstanceOf(CliError);
    expect((failure as CliError).message).toContain('unknown key "runtime"');
    expect((failure as CliError).message).toContain(CONTROLLER_CONFIG_EXAMPLE);
    expect(f.fetches).toEqual([]);
  });

  it("refuses a missing required key", async () => {
    const f = await fixture({
      yaml: [
        "project: demo",
        `daemon_url: ${DAEMON_URL}`,
        "operator_token_file: ./operator-token",
        "envoy_url: http://envoy.test:9020",
      ],
    });
    const failure = await cmdControllerStart({ configPath: f.configPath }, f.deps).catch(
      (error) => error
    );
    expect((failure as CliError).message).toBe(
      "nats_urls is required in the controller configuration"
    );
  });

  it("requires dispatch_url and dispatch_token_file together", () => {
    const base = [
      "project: demo",
      `daemon_url: ${DAEMON_URL}`,
      "operator_token_file: ./operator-token",
      "envoy_url: http://envoy.test:9020",
      "nats_urls: [nats://a:4222]",
    ];
    expect(() =>
      loadControllerStartConfig([...base, "dispatch_url: https://d.test"].join("\n"), "/cfg")
    ).toThrow("dispatch_url is set but dispatch_token_file is not");
    expect(() =>
      loadControllerStartConfig([...base, "dispatch_token_file: ./t"].join("\n"), "/cfg")
    ).toThrow("dispatch_token_file is set but dispatch_url is not");
    expect(() =>
      loadControllerStartConfig(
        [...base, "dispatch_url: https://d.test/mcp", "dispatch_token_file: ./t"].join("\n"),
        "/cfg"
      )
    ).toThrow("dispatch_url must be the dispatch service base URL, not the /mcp endpoint");
  });

  it("resolves state_dir and every file key against the configuration file's directory, without tilde expansion", () => {
    const config = loadControllerStartConfig(
      [
        "project: demo",
        `daemon_url: ${DAEMON_URL}/`,
        "operator_token_file: operator-token",
        "envoy_url: http://envoy.test:9020",
        "envoy_token_file: ./envoy-token",
        "nats_urls: [nats://a:4222]",
        "dispatch_url: https://d.test/",
        "dispatch_token_file: ~/dispatch-token",
        "instructions: ./instructions.md",
        "state_dir: ./state",
      ].join("\n"),
      "/cfg"
    );
    expect(config).toEqual({
      project: "demo",
      daemonUrl: `${DAEMON_URL}/`,
      operatorTokenFile: "/cfg/operator-token",
      envoyUrl: "http://envoy.test:9020",
      envoyTokenFile: "/cfg/envoy-token",
      natsUrls: ["nats://a:4222"],
      dispatchUrl: "https://d.test",
      dispatchTokenFile: "/cfg/~/dispatch-token",
      instructions: "/cfg/instructions.md",
      ompInvocation: expect.stringMatching(/^mise x .* -- omp$/),
      ompLaunchPrefix: [],
      stateDir: "/cfg/state",
    });
  });

  it("refuses a file that is not a mapping", () => {
    expect(() => loadControllerStartConfig("- a\n- b\n", "/cfg")).toThrow(
      "controller.yaml must be a mapping"
    );
  });

  it("uses state_dir from the file when set", async () => {
    const f = await fixture({
      yaml: [
        "project: demo",
        `daemon_url: ${DAEMON_URL}`,
        "operator_token_file: ./operator-token",
        "envoy_url: http://envoy.test:9020",
        "nats_urls: [nats://a:4222]",
        "state_dir: ./state",
      ],
    });
    await cmdControllerStart({ configPath: f.configPath }, f.deps);
    expect(f.spawns[0]?.env.LEGION_STATE_DIR).toBe(path.join(f.dir, "state"));
    expect(existsSync(path.join(f.dir, "state", "secrets", "legion-demo-controller"))).toBe(true);
  });
});

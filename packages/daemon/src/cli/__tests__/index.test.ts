import { afterAll, describe, expect, it, spyOn } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { CommandRunner } from "../../state/fetch";
import {
  cmdCheckConfig,
  cmdCredential,
  cmdGh,
  cmdHandoffComplete,
  cmdProbeImage,
  loadStartConfig,
  resolveControllerSecret,
} from "../index";

const grantDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-cli-grant-"));
afterAll(() => fs.rmSync(grantDir, { recursive: true, force: true }));

/** A `LEGION_GRANT_FILE` as the pi-envoy extension leaves it on a pane: a 0600 file holding the
 * grant minted for the command that is about to run. */
function grantFile(contents: string): string {
  const file = path.join(grantDir, `grant-${crypto.randomUUID()}`);
  fs.writeFileSync(file, contents, { mode: 0o600 });
  return file;
}

describe("grant resolution", () => {
  const credentialDeps = (env: NodeJS.ProcessEnv, onFetch: (request: Request) => void) => ({
    env,
    readStdin: async () => "protocol=https\nhost=github.com\n",
    write: () => {},
    fetch: async (input: string | URL | Request, init?: RequestInit) => {
      onFetch(new Request(String(input), init));
      return new Response("username=x-access-token\npassword=ghs_token\n");
    },
  });

  it("reads LEGION_GRANT_FILE (trimmed) ahead of LEGION_GRANT", async () => {
    let request: Request | undefined;
    await cmdCredential(
      credentialDeps(
        { LEGION_GRANT_FILE: grantFile("  file-grant\n"), LEGION_GRANT: "env-grant" },
        (r) => {
          request = r;
        }
      )
    );
    expect(new URL(request?.url ?? "").pathname).toBe("/legion/v1/git-credential");
    expect(await request?.json()).toEqual({ grantId: "file-grant" });
  });

  it("errors naming LEGION_GRANT_FILE and the path when the file is missing or blank, without falling back", async () => {
    let fetched = false;
    const missing = path.join(grantDir, "missing-grant");
    await expect(
      cmdCredential(
        credentialDeps({ LEGION_GRANT_FILE: missing, LEGION_GRANT: "env-grant" }, () => {
          fetched = true;
        })
      )
    ).rejects.toEqual(
      expect.objectContaining({
        message: expect.stringMatching(
          new RegExp(
            `^LEGION_GRANT_FILE names ${missing.replaceAll(".", "\\.")}, which could not be read: .*ENOENT.*: the pi-envoy extension in this pane did not write it — the installed plugin predates LEGION-54`
          )
        ),
        code: 1,
      })
    );
    const blank = grantFile(" \n");
    await expect(
      cmdCredential(
        credentialDeps({ LEGION_GRANT_FILE: blank, LEGION_GRANT: "env-grant" }, () => {
          fetched = true;
        })
      )
    ).rejects.toEqual(
      expect.objectContaining({
        message: `LEGION_GRANT_FILE names ${blank}, which is empty`,
        code: 1,
      })
    );
    expect(fetched).toBe(false);
  });

  it("names both variables when neither is set", async () => {
    let fetched = false;
    await expect(
      cmdCredential(
        credentialDeps({}, () => {
          fetched = true;
        })
      )
    ).rejects.toEqual(
      expect.objectContaining({
        message: expect.stringContaining(
          "LEGION_GRANT_FILE is missing (and LEGION_GRANT is unset)"
        ),
        code: 1,
      })
    );
    expect(fetched).toBe(false);
  });

  it("falls back to LEGION_GRANT only when no LEGION_GRANT_FILE is set on the pane", async () => {
    let request: Request | undefined;
    await cmdCredential(
      credentialDeps({ LEGION_GRANT: "env-grant" }, (r) => {
        request = r;
      })
    );
    expect(await request?.json()).toEqual({ grantId: "env-grant" });
  });
});
describe("legion gh", () => {
  function ghDeps() {
    const fetchCalls: Request[] = [];
    return {
      fetchCalls,
      env: { LEGION_GRANT: "grant-1" },
      fetch: async (input: string | URL | Request, init?: RequestInit) => {
        fetchCalls.push(new Request(String(input), init));
        return Response.json({ token: "scoped-token", appLogin: "legion-implementer[bot]" });
      },
      spawnGh: async () => 0,
    };
  }

  it("redeems the worker-extension grant only into the gh child environment", async () => {
    let request: Request | undefined;
    let spawnArgs: string[] | undefined;
    let childEnvironment: NodeJS.ProcessEnv | undefined;
    const parentToken = process.env.GH_TOKEN;
    delete process.env.GH_TOKEN;

    try {
      await cmdGh(["api", "user"], {
        env: {
          LEGION_GRANT_FILE: grantFile("grant-123"),
          GITHUB_TOKEN: "personal-github-token",
        },
        fetch: async (input, init) => {
          request = new Request(String(input), init);
          return Response.json({ token: "scoped-token", appLogin: "legion-implementer[bot]" });
        },
        spawnGh: async (args, env) => {
          spawnArgs = args;
          childEnvironment = env;
          return 0;
        },
      });

      expect(await request?.json()).toEqual({ grantId: "grant-123" });
      expect(new URL(request?.url ?? "").pathname).toBe("/legion/v1/gh-token");
      expect(spawnArgs).toEqual(["api", "user"]);
      expect(childEnvironment?.GH_TOKEN).toBe("scoped-token");
      expect(childEnvironment?.GITHUB_TOKEN).toBeUndefined();
      expect(childEnvironment?.GH_CONFIG_DIR).toMatch(/[/\\]legion[/\\]gh$/);
      expect(childEnvironment?.GH_CONFIG_DIR).not.toBe("/dev/null");
      expect(process.env.GH_TOKEN).toBeUndefined();
    } finally {
      if (parentToken === undefined) delete process.env.GH_TOKEN;
      else process.env.GH_TOKEN = parentToken;
    }
  });

  it("spawns the real gh, never the pane's worker-bin shim: the child PATH drops every worker-bin entry", async () => {
    // A pane's PATH puts <state_dir>/worker-bin (the `gh` shim that execs `legion gh`) first for
    // the pane's life. Left in place, `legion gh`'s own `gh` child would resolve to that shim and
    // re-enter `legion gh` under a child env whose grant pointer is already scrubbed.
    let childEnvironment: NodeJS.ProcessEnv | undefined;
    await cmdGh(["--version"], {
      env: {
        LEGION_GRANT_FILE: grantFile("grant-123"),
        LEGION_STATE_DIR: "/state",
        PATH: `/state/worker-bin${path.delimiter}/state/bin${path.delimiter}/usr/bin`,
      },
      fetch: async () =>
        Response.json({ token: "scoped-token", appLogin: "legion-implementer[bot]" }),
      spawnGh: async (_args, env) => {
        childEnvironment = env;
        return 0;
      },
    });
    expect(childEnvironment?.PATH).toBe(`/state/bin${path.delimiter}/usr/bin`);
    expect(childEnvironment?.LEGION_GRANT_FILE).toBeUndefined();
  });

  it("fails loudly when the pane carries no grant file and no grant", async () => {
    await expect(
      cmdGh(["api", "user"], {
        env: {},
        fetch: async () => Response.json({ token: "unused" }),
        spawnGh: async () => 0,
      })
    ).rejects.toEqual(
      expect.objectContaining({
        message: expect.stringContaining("LEGION_GRANT_FILE is missing"),
        code: 1,
      })
    );
  });

  it("refuses every merge-shaped gh invocation before any daemon call, for every role", async () => {
    for (const args of [
      ["pr", "merge", "7", "--squash"],
      ["pr", "merge", "--repo", "o/r", "7"],
      ["api", "-X", "PUT", "repos/o/r/pulls/7/merge"],
      [
        "api",
        "graphql",
        "-f",
        "query=mutation { mergePullRequest(input:{}) { clientMutationId } }",
      ],
      ["alias", "set", "m", "pr merge"],
    ]) {
      const deps = ghDeps();
      await expect(cmdGh(args, deps)).rejects.toThrow(
        "Legion never merges a pull request: publish READY (merger role) and let a human merge under the repository's code-owner rule"
      );
      expect(deps.fetchCalls).toHaveLength(0);
    }
  });

  it("redeems the grant without merge intent for an ordinary pr subcommand", async () => {
    const deps = ghDeps();
    await cmdGh(["pr", "view", "7", "--json", "mergeable"], deps);
    expect(await deps.fetchCalls[0]?.json()).toEqual({ grantId: "grant-1" });
  });

  it("refuses gh issue comment before any grant is redeemed: Legion issues live on Dispatch", async () => {
    let fetchCalled = false;
    let spawnCalled = false;

    await expect(
      cmdGh(["issue", "comment", "27", "--repo", "sjawhar/legion", "--body", "## Retro Complete"], {
        env: { LEGION_GRANT: "grant-123", LEGION_ISSUE: "LEGION-27" },
        fetch: async () => {
          fetchCalled = true;
          return Response.json({ token: "unused", appLogin: "legion-implementer[bot]" });
        },
        spawnGh: async () => {
          spawnCalled = true;
          return 0;
        },
      })
    ).rejects.toEqual(
      expect.objectContaining({
        message:
          "Legion issues live on Dispatch; use dispatch_message or dispatch_comment on LEGION-27",
        code: 1,
      })
    );
    expect(fetchCalled).toBe(false);
    expect(spawnCalled).toBe(false);
  });

  it("rejects the --repo bypass of the issue-write guard and names the issue generically without LEGION_ISSUE", async () => {
    let fetchCalled = false;
    let spawnCalled = false;

    await expect(
      cmdGh(["issue", "--repo", "acme/widgets", "comment", "5"], {
        env: { LEGION_GRANT: "grant-123" },
        fetch: async () => {
          fetchCalled = true;
          return Response.json({ token: "unused", appLogin: "legion-implementer[bot]" });
        },
        spawnGh: async () => {
          spawnCalled = true;
          return 0;
        },
      })
    ).rejects.toEqual(
      expect.objectContaining({
        message:
          "Legion issues live on Dispatch; use dispatch_message or dispatch_comment on the Dispatch issue",
        code: 1,
      })
    );
    expect(fetchCalled).toBe(false);
    expect(spawnCalled).toBe(false);
  });

  it.each([
    "comment",
    "create",
    "edit",
    "close",
    "reopen",
    "delete",
    "pin",
    "unpin",
    "transfer",
    "lock",
    "unlock",
    "develop",
  ])("refuses every gh issue write verb: %s", async (verb) => {
    let fetchCalled = false;
    let spawnCalled = false;

    await expect(
      cmdGh(["issue", verb, "5"], {
        env: { LEGION_GRANT: "grant-123" },
        fetch: async () => {
          fetchCalled = true;
          return Response.json({ token: "unused", appLogin: "legion-implementer[bot]" });
        },
        spawnGh: async () => {
          spawnCalled = true;
          return 0;
        },
      })
    ).rejects.toEqual(
      expect.objectContaining({
        message: expect.stringMatching(/^Legion issues live on Dispatch/),
        code: 1,
      })
    );
    expect(fetchCalled).toBe(false);
    expect(spawnCalled).toBe(false);
  });

  it.each([
    // An explicit method.
    [["api", "-X", "POST", "repos/acme/widgets/issues/5/comments", "-f", "body=hi"]],
    // Creates an issue; `-f` alone makes gh POST.
    [["api", "repos/acme/widgets/issues", "-f", "title=x"]],
    // `--input` alone makes gh POST.
    [["api", "repos/acme/widgets/issues/5/comments", "--input", "body.json"]],
    // Attached method value.
    [["api", "--method=PATCH", "repos/acme/widgets/issues/5", "-F", "state=closed"]],
    // Attached short method; a PR conversation comment by raw API is refused by design.
    [["api", "-XDELETE", "repos/acme/widgets/issues/comments/99"]],
    // A full URL.
    [["api", "-X", "PUT", "https://api.github.com/repos/acme/widgets/issues/5/lock"]],
  ])("refuses a raw gh api write to an /issues path: %j", async (args) => {
    let fetchCalled = false;
    let spawnCalled = false;

    await expect(
      cmdGh(args, {
        env: { LEGION_GRANT: "grant-123" },
        fetch: async () => {
          fetchCalled = true;
          return Response.json({ token: "unused", appLogin: "legion-implementer[bot]" });
        },
        spawnGh: async () => {
          spawnCalled = true;
          return 0;
        },
      })
    ).rejects.toEqual(
      expect.objectContaining({
        message: expect.stringMatching(/^Legion issues live on Dispatch/),
        code: 1,
      })
    );
    expect(fetchCalled).toBe(false);
    expect(spawnCalled).toBe(false);
  });

  it.each([
    [["pr", "comment", "5", "--body", "Verification complete.", "--repo", "acme/widgets"]],
    [["pr", "review", "5", "--approve"]],
    [["api", "--method", "POST", "repos/acme/widgets/pulls/5/reviews", "--input", "body.json"]],
    [["issue", "view", "5", "--repo", "acme/widgets", "--json", "title"]],
    [["issue", "list", "--state", "open"]],
    // A GET on an issues path.
    [["api", "repos/acme/widgets/issues/5/comments", "--jq", "length"]],
    // A GET with query parameters: `-f` does not make an explicit GET a write.
    [["api", "--method", "GET", "repos/acme/widgets/issues", "-f", "state=open"]],
  ])("still forwards %j", async (args) => {
    let fetchCalled = false;
    let spawnArgs: string[] | undefined;

    await cmdGh(args, {
      env: { LEGION_GRANT_FILE: grantFile("grant-123"), LEGION_ISSUE: "LEGION-78" },
      fetch: async () => {
        fetchCalled = true;
        return Response.json({ token: "scoped-token", appLogin: "legion-implementer[bot]" });
      },
      spawnGh: async (spawned) => {
        spawnArgs = spawned;
        return 0;
      },
    });

    expect(fetchCalled).toBe(true);
    expect(spawnArgs).toEqual(args);
  });
});
describe("legion start --check-config", () => {
  // The env is an input to cmdCheckConfig, never read from the process: a Legion worker pane
  // exports DISPATCH_URL and DISPATCH_TOKEN_FILE without DISPATCH_TOKEN, which alone would make
  // resolveDaemonConfig refuse the file under test. PATH and HOME give the fixture a realistic
  // shape; resolveDaemonConfig reads neither.
  const env: NodeJS.ProcessEnv = { PATH: process.env.PATH, HOME: process.env.HOME };
  /** Every non-App key a `legion.yaml` under test needs; each case appends its `github_apps`. */
  const baseYaml = [
    "project: acme/99",
    "envoy_url: http://127.0.0.1:9020",
    "projects:",
    "  ACME: { repo: acme/widgets }",
    "nats_urls:",
    "  - nats://one:4222",
    "gates:",
    "  design: off",
  ];
  const reviewAppYaml = ["  review:", '    app_id: "2"', '    private_key: "test"'];
  const bothAppsYaml = [
    "github_apps:",
    "  implement:",
    '    app_id: "1"',
    '    private_key: "test"',
    ...reviewAppYaml,
  ];
  function writeYaml(dir: string, lines: string[]): string {
    const configPath = path.join(dir, "legion.yaml");
    fs.writeFileSync(configPath, lines.join("\n"));
    return configPath;
  }

  it("validates github_apps.<role>.private_key_command without executing it", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-check-config-"));
    const marker = path.join(dir, "spawned");
    const configPath = writeYaml(dir, [
      ...baseYaml,
      "github_apps:",
      "  implement:",
      '    app_id: "1"',
      `    private_key_command: "touch ${marker}; printf key"`,
      ...reviewAppYaml,
    ]);

    await cmdCheckConfig(undefined, configPath, env);

    expect(fs.existsSync(marker)).toBe(false);
  });

  it("validates github_apps.<role>.private_key_secret without running secrets", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-check-config-"));
    const fakeBin = path.join(dir, "bin");
    fs.mkdirSync(fakeBin);
    const marker = path.join(dir, "spawned");
    fs.writeFileSync(path.join(fakeBin, "secrets"), `#!/bin/sh\ntouch '${marker}'\nexit 1\n`, {
      mode: 0o755,
    });
    const configPath = writeYaml(dir, [
      ...baseYaml,
      "github_apps:",
      "  implement:",
      '    app_id: "1"',
      "    private_key_secret: GH_AGENT_APP_PRIVATE_KEY_B64",
      ...reviewAppYaml,
    ]);
    // The resolver spawns `secrets` from process.env.PATH (like private_key_command's `sh -c`),
    // so the fake goes first on the real PATH; a spawn under --check-config would leave the marker.
    const savedPath = process.env.PATH;
    process.env.PATH = `${fakeBin}${path.delimiter}${savedPath}`;
    try {
      await cmdCheckConfig(undefined, configPath, {
        PATH: process.env.PATH,
        HOME: process.env.HOME,
      });
    } finally {
      process.env.PATH = savedPath;
    }

    expect(fs.existsSync(marker)).toBe(false);
  });

  it("still rejects a github app with no private-key source, naming all three", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-check-config-"));
    const configPath = writeYaml(dir, [
      ...baseYaml,
      "github_apps:",
      "  implement:",
      '    app_id: "1"',
      ...reviewAppYaml,
    ]);

    await expect(cmdCheckConfig(undefined, configPath, env)).rejects.toThrow(
      "github_apps.implement requires exactly one of private_key, private_key_command, or private_key_secret"
    );
  });

  it("rejects a legion.yaml with only the implement App, naming github_apps.review", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-check-config-"));
    const configPath = writeYaml(dir, [
      ...baseYaml,
      "github_apps:",
      "  implement:",
      '    app_id: "1"',
      '    private_key: "test"',
    ]);

    await expect(cmdCheckConfig(undefined, configPath, env)).rejects.toThrow(
      "github_apps.review is required"
    );
  });

  it("resolves Dispatch settings from the injected env, not the process environment", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-check-config-"));
    const configPath = writeYaml(dir, [...baseYaml, ...bothAppsYaml]);

    await expect(
      cmdCheckConfig(undefined, configPath, { ...env, DISPATCH_URL: "http://127.0.0.1:1" })
    ).rejects.toThrow(
      "dispatch_url is set but DISPATCH_TOKEN is not; the dispatch tools would not register"
    );

    await cmdCheckConfig(undefined, configPath, {
      ...env,
      DISPATCH_URL: "http://127.0.0.1:1",
      DISPATCH_TOKEN: "t",
    });
  });

  it("refuses a timer setting above the 32-bit bound with the same message start-up gives", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-check-config-"));
    const configPath = writeYaml(dir, [
      ...baseYaml,
      ...bothAppsYaml,
      "worker_boot_timeout_seconds: 2147484",
    ]);

    await expect(cmdCheckConfig(undefined, configPath, env)).rejects.toThrow(
      "worker_boot_timeout_seconds must be at most 2147483"
    );

    // The same parser accepts the bound itself; the registration deadline multiplies the boot
    // timeout by the intervals, so the default 3 intervals would overflow at the bound.
    writeYaml(dir, [
      ...baseYaml,
      ...bothAppsYaml,
      "worker_boot_timeout_seconds: 2147483",
      "worker_boot_registration_deadline_intervals: 1",
    ]);
    await cmdCheckConfig(undefined, configPath, env);
  });

  it("resolves relative instructions and state_dir against the --config file's directory, not the cwd", async () => {
    const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "legion-check-config-")));
    const configPath = writeYaml(dir, [
      ...baseYaml,
      ...bothAppsYaml,
      "state_dir: ./state",
      "instructions: ./ops/deployment.md",
    ]);
    expect(process.cwd()).not.toBe(dir);

    const config = loadStartConfig(undefined, configPath, env, { resolveSecrets: false });

    expect(config.stateDir).toBe(path.join(dir, "state"));
    expect(config.instructionsPath).toBe(path.join(dir, "ops", "deployment.md"));
  });

  it("checks an in-cluster legion.yaml on a machine without its Secret mounts: envoy_token_file and operator_token_file are validated, never read", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-check-config-"));
    const configPath = writeYaml(dir, [
      ...baseYaml,
      ...bothAppsYaml,
      "runtime:",
      "  kubernetes:",
      "    namespace: legion",
      `    image: ghcr.io/sjawhar/legion-worker@sha256:${"a".repeat(64)}`,
      "bind: 0.0.0.0",
      "daemon_url: http://legion-daemon-acme.legion.svc:13370",
      `envoy_token_file: ${path.join(dir, "no-such-mount", "ENVOY_TOKEN")}`,
      `operator_token_file: ${path.join(dir, "no-such-mount", "OPERATOR_TOKEN")}`,
    ]);

    await cmdCheckConfig(undefined, configPath, env);

    // The daemon itself still reads them: the same file is a boot refusal naming the key and path.
    expect(() => loadStartConfig(undefined, configPath, env)).toThrow(
      `envoy_token_file names ${path.join(dir, "no-such-mount", "ENVOY_TOKEN")}, which could not be read: ENOENT`
    );
  });
});

describe("legion handoff complete", () => {
  it("posts the phase-complete request built from the grant in LEGION_GRANT_FILE", async () => {
    let request: Request | undefined;

    await cmdHandoffComplete("Verified the acceptance criteria end to end.", {
      env: { LEGION_GRANT_FILE: grantFile("grant-123") },
      fetch: async (input, init) => {
        request = new Request(String(input), init);
        return Response.json({});
      },
    });

    expect(new URL(request?.url ?? "").pathname).toBe("/legion/v1/phase/complete");
    expect(await request?.json()).toEqual({
      grantId: "grant-123",
      summary: "Verified the acceptance criteria end to end.",
    });
  });

  it("fails loudly when the pane carries no grant file and no grant", async () => {
    await expect(
      cmdHandoffComplete("smoke", {
        env: {},
        fetch: async () => Response.json({}),
      })
    ).rejects.toEqual(
      expect.objectContaining({
        message: expect.stringContaining("LEGION_GRANT_FILE is missing"),
        code: 1,
      })
    );
  });

  it("fails loudly when the daemon rejects the request", async () => {
    await expect(
      cmdHandoffComplete("smoke", {
        env: { LEGION_GRANT_FILE: grantFile("grant-123") },
        fetch: async () => new Response("Stale worker generation", { status: 409 }),
      })
    ).rejects.toEqual(
      expect.objectContaining({
        message: expect.stringContaining("409"),
        code: 1,
      })
    );
  });

  it("warns instead of reporting plain success when the daemon returns 202 for no live architect holder", async () => {
    const messages: string[] = [];
    const originalLog = console.log;
    console.log = (message: string) => messages.push(message);
    try {
      await cmdHandoffComplete("smoke", {
        env: { LEGION_GRANT_FILE: grantFile("grant-123") },
        fetch: async () => new Response(JSON.stringify({}), { status: 202 }),
      });
    } finally {
      console.log = originalLog;
    }

    expect(messages).toEqual([
      "[handoff] Warning: phase recorded; no architect was live to receive the summary",
    ]);
  });
});

describe("resolveControllerSecret", () => {
  it("reads LEGION_CONTROLLER_SECRET_FILE (trimmed) ahead of LEGION_CONTROLLER_SECRET", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-cli-secret-"));
    const file = path.join(dir, "controller");
    fs.writeFileSync(file, "  file-secret\n");
    try {
      expect(
        resolveControllerSecret({
          LEGION_CONTROLLER_SECRET_FILE: file,
          LEGION_CONTROLLER_SECRET: "env-secret",
        })
      ).toBe("file-secret");
      expect(resolveControllerSecret({ LEGION_CONTROLLER_SECRET: "env-secret" })).toBe(
        "env-secret"
      );
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  it("fails naming the variable and path when the file is missing or empty, never falling back", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-cli-secret-"));
    const empty = path.join(dir, "empty");
    fs.writeFileSync(empty, " \n");
    try {
      expect(() =>
        resolveControllerSecret({
          LEGION_CONTROLLER_SECRET_FILE: path.join(dir, "missing"),
          LEGION_CONTROLLER_SECRET: "env-secret",
        })
      ).toThrow(
        `LEGION_CONTROLLER_SECRET_FILE names ${path.join(dir, "missing")}, which could not be read`
      );
      expect(() =>
        resolveControllerSecret({
          LEGION_CONTROLLER_SECRET_FILE: empty,
          LEGION_CONTROLLER_SECRET: "env-secret",
        })
      ).toThrow(`LEGION_CONTROLLER_SECRET_FILE names ${empty}, which is empty`);
      expect(() => resolveControllerSecret({})).toThrow(
        "LEGION_CONTROLLER_SECRET or LEGION_CONTROLLER_SECRET_FILE is required for controller commands"
      );
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe("legion probe-image", () => {
  const deps = (runner: CommandRunner, env: NodeJS.ProcessEnv) => ({
    env,
    runner,
    sleep: async () => {},
    readPluginManifest: async () => "{}",
  });
  /** A build every probe accepts: three marker probes and a refusal naming
   * `OMP_SESSION_STORAGE` — exit 1 — on the session-storage probe, whose pass is that refusal. */
  const passing = async (command: string[]) =>
    command[2]?.includes("OMP_SESSION_STORAGE=")
      ? {
          stdout: "",
          stderr: 'Error: OMP_SESSION_STORAGE is "legion-launch-probe"; expected "file" or "sql"\n',
          exitCode: 1,
        }
      : {
          stdout: "",
          stderr:
            "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\nLEGION_OMP_PROMPT_DEPENDENCIES=resolved\n",
          exitCode: 0,
        };

  it("refuses to probe without an explicit OMP executable (no PATH fallback)", async () => {
    let ran = false;
    await expect(
      cmdProbeImage(
        undefined,
        deps(async (command) => {
          ran = true;
          return passing(command);
        }, {})
      )
    ).rejects.toEqual(
      expect.objectContaining({ message: expect.stringContaining("LEGION_OMP_PATH"), code: 1 })
    );
    expect(ran).toBe(false);
  });

  it("runs the daemon's four launch probes against LEGION_OMP_PATH with no launch prefix and marks the success line", async () => {
    const commands: string[][] = [];
    const lines: string[] = [];
    const logSpy = spyOn(console, "log").mockImplementation((...args: unknown[]) => {
      lines.push(args.map(String).join(" "));
    });
    try {
      await cmdProbeImage(
        undefined,
        deps(
          async (command) => {
            commands.push(command);
            return passing(command);
          },
          { LEGION_OMP_PATH: "/opt/omp/bin/omp" }
        )
      );
    } finally {
      logSpy.mockRestore();
    }
    expect(commands).toHaveLength(4);
    expect(commands[0]?.[2]).toStartWith(
      'exec /opt/omp/bin/omp models --no-extensions --extension "$1" --json'
    );
    expect(commands[1]?.[2]).toStartWith('exec /opt/omp/bin/omp models --extension "$1" --json');
    expect(commands[2]?.[2]).toStartWith(
      'LEGION_PROMPT_DEPENDENCIES="$1" exec /opt/omp/bin/omp models --extension "$2" --json'
    );
    expect(commands[3]?.[2]).toStartWith(
      "export OMP_SESSION_STORAGE=legion-launch-probe PI_TIMING=x; exec /opt/omp/bin/omp "
    );
    // The token a daemon accepting this image for a sql session store requires in the probe
    // pod's output: an older image's command prints a bare `probe-image: OK`.
    expect(lines).toEqual(["probe-image: OK (/opt/omp/bin/omp) session-storage=probed"]);
  });

  it("refuses an image whose OMP starts on a nonsense OMP_SESSION_STORAGE: the build predates the setting", async () => {
    const commands: string[][] = [];
    await expect(
      cmdProbeImage(
        "/opt/omp/bin/omp",
        deps(async (command) => {
          commands.push(command);
          // The marker probes pass; the session-storage probe's launch exits 0 with the timing tree.
          return command[2]?.includes("OMP_SESSION_STORAGE=")
            ? { stdout: "", stderr: "Total: 4925.1ms (since first marker)\n", exitCode: 0 }
            : passing(command);
        }, {})
      )
    ).rejects.toEqual(
      expect.objectContaining({
        message: expect.stringContaining(
          "predates the session.storage setting and would silently keep sessions on files"
        ),
        code: 1,
      })
    );
    expect(commands).toHaveLength(4);
  });

  it("surfaces a failing probe as the daemon's own message with exit 1", async () => {
    await expect(
      cmdProbeImage(
        "/opt/omp/bin/omp",
        deps(async () => ({ stdout: "", stderr: "LEGION_OMP_AGENTS=missing\n", exitCode: 0 }), {})
      )
    ).rejects.toEqual(
      expect.objectContaining({
        message: expect.stringContaining("does not expose pi.agents"),
        code: 1,
      })
    );
  });

  it.each([
    ["pi.agents", "LEGION_OMP_AGENTS=missing\n", "does not expose pi.agents", 1],
    ["plugin load", "LEGION_PLUGIN_LOADED=no\n", "is installed but not loaded by omp", 2],
  ])("treats a %s probe that prints its negative marker and then hangs past the budget as definitive: no retry", async (_probe, stderr, message, expectedAttempts) => {
    let attempts = 0;
    const sleeps: number[] = [];
    await expect(
      cmdProbeImage("/opt/omp/bin/omp", {
        env: {},
        runner: async (command) => {
          attempts += 1;
          // In the plugin-load case the pi.agents probe (the `--no-extensions` one) passes first.
          if (_probe === "plugin load" && command[2]?.includes("--no-extensions")) {
            return { stdout: "", stderr: "LEGION_OMP_AGENTS=available\n", exitCode: 0 };
          }
          // The negative marker is already on stderr when the runner's kill fires: the answer
          // is in, and no retry can change it.
          return {
            stdout: "",
            stderr,
            exitCode: 143,
            timedOut: { limitMs: 300_000, elapsedMs: 300_100 },
          };
        },
        sleep: async (ms) => {
          sleeps.push(ms);
        },
        readPluginManifest: async () => "{}",
      })
    ).rejects.toEqual(
      expect.objectContaining({ message: expect.stringContaining(message), code: 1 })
    );
    expect(sleeps).toEqual([]);
    expect(attempts).toBe(expectedAttempts);
  });

  it("gives up after the bounded retry when every probe attempt times out", async () => {
    const sleeps: number[] = [];
    let attempts = 0;
    const errorSpy = spyOn(console, "error").mockImplementation(() => {});
    try {
      await expect(
        cmdProbeImage("/opt/omp/bin/omp", {
          env: {},
          runner: async (command) => {
            attempts += 1;
            expect(command).toEqual(expect.arrayContaining(["sh", "-c"]));
            return {
              stdout: "",
              stderr: "",
              exitCode: 143,
              timedOut: { limitMs: 300_000, elapsedMs: 300_100 },
            };
          },
          sleep: async (ms) => {
            sleeps.push(ms);
          },
          readPluginManifest: async () => "{}",
        })
      ).rejects.toEqual(
        expect.objectContaining({
          message: expect.stringContaining(
            "OMP pi.agents probe never completed within its retry budget (6 attempts)"
          ),
          code: 1,
        })
      );
    } finally {
      errorSpy.mockRestore();
    }
    expect(sleeps).toEqual([10_000, 20_000, 40_000, 80_000, 160_000]);
    expect(attempts).toBe(6);
  });

  describe("--daemon-api-version", () => {
    const manifest = JSON.stringify({ version: "0.9.1", legion: { daemonApiVersion: 7 } });
    const contractDeps = (manifestText: string) => ({
      env: {},
      runner: passing,
      sleep: async () => {},
      readPluginManifest: async () => manifestText,
    });

    it("passes and reports the contract when the image plugin's manifest matches the flag", async () => {
      const lines: string[] = [];
      const logSpy = spyOn(console, "log").mockImplementation((line: string) => {
        lines.push(line);
      });
      try {
        await cmdProbeImage("/opt/omp/bin/omp", contractDeps(manifest), {
          daemonApiVersion: "7",
        });
      } finally {
        logSpy.mockRestore();
      }
      expect(lines).toEqual([
        "probe-image: OK (/opt/omp/bin/omp) session-storage=probed daemon-api-version=7",
      ]);
    });

    it("exits 1 naming both versions and the manifest path when the image plugin speaks another contract", async () => {
      await expect(
        cmdProbeImage("/opt/omp/bin/omp", contractDeps(manifest), { daemonApiVersion: "8" })
      ).rejects.toEqual(
        expect.objectContaining({
          code: 1,
          message: expect.stringMatching(
            /pi-legion-envoy at \S*@sjawhar\/pi-legion-envoy\/package\.json \(package 0\.9\.1\) speaks daemon API contract 7; this daemon requires 8/
          ),
        })
      );
    });

    it.each([
      "0",
      "-1",
      "abc",
      "1.5",
      "",
    ])("refuses %j as a contract version, naming the flag, before any probe runs", async (value) => {
      let ran = false;
      await expect(
        cmdProbeImage(
          "/opt/omp/bin/omp",
          {
            ...contractDeps(manifest),
            runner: async (command) => {
              ran = true;
              return passing(command);
            },
          },
          { daemonApiVersion: value }
        )
      ).rejects.toEqual(
        expect.objectContaining({
          code: 1,
          message: expect.stringContaining("--daemon-api-version must be a positive integer"),
        })
      );
      expect(ran).toBe(false);
    });

    it("never reads the manifest for the contract when the flag is absent", async () => {
      let manifestReads = 0;
      await cmdProbeImage("/opt/omp/bin/omp", {
        ...contractDeps(manifest),
        readPluginManifest: async () => {
          manifestReads += 1;
          return manifest;
        },
      });
      expect(manifestReads).toBe(0);
    });
  });
});

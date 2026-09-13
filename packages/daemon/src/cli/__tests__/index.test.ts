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

  it("refuses to merge a PR: no Legion worker role ever merges directly", async () => {
    let fetchCalled = false;
    let spawnCalled = false;

    await expect(
      cmdGh(["pr", "merge", "123", "--squash"], {
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
        message: "Legion workers never merge; publish READY to the merge queue",
        code: 1,
      })
    );
    expect(fetchCalled).toBe(false);
    expect(spawnCalled).toBe(false);
  });

  it("rejects the --repo bypass of the pr-merge guard: `gh pr --repo <value> merge`", async () => {
    let fetchCalled = false;
    let spawnCalled = false;

    await expect(
      cmdGh(["pr", "--repo", "acme/widgets", "merge", "123"], {
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
        message: "Legion workers never merge; publish READY to the merge queue",
        code: 1,
      })
    );
    expect(fetchCalled).toBe(false);
    expect(spawnCalled).toBe(false);
  });

  it("rejects the REST bypass of the pr-merge guard: `gh api .../pulls/<n>/merge`", async () => {
    let fetchCalled = false;
    let spawnCalled = false;

    await expect(
      cmdGh(["api", "-X", "PUT", "repos/acme/widgets/pulls/123/merge"], {
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
        message: "Legion workers never merge; publish READY to the merge queue",
        code: 1,
      })
    );
    expect(fetchCalled).toBe(false);
    expect(spawnCalled).toBe(false);
  });

  it("allows a pr subcommand that merely mentions merge in an unrelated argument", async () => {
    let spawnArgs: string[] | undefined;

    await cmdGh(["pr", "view", "merge-fix"], {
      env: { LEGION_GRANT_FILE: grantFile("grant-123") },
      fetch: async () =>
        Response.json({ token: "scoped-token", appLogin: "legion-implementer[bot]" }),
      spawnGh: async (args) => {
        spawnArgs = args;
        return 0;
      },
    });

    expect(spawnArgs).toEqual(["pr", "view", "merge-fix"]);
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
    [
      [
        "api",
        "graphql",
        "-f",
        'query=mutation { resolveReviewThread(input: {threadId: "x"}) { thread { id } } }',
      ],
    ],
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

  it("validates github_apps.<role>.private_key_command without executing it", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-check-config-"));
    const marker = path.join(dir, "spawned");
    const configPath = path.join(dir, "legion.yaml");
    fs.writeFileSync(
      configPath,
      [
        "project: acme/99",
        "envoy_url: http://127.0.0.1:9020",
        "dispatch_project: ACME",
        "repos:",
        "  - acme/widgets",
        "nats_urls:",
        "  - nats://one:4222",
        "gates:",
        "  design: off",
        "github_apps:",
        "  implement:",
        '    app_id: "1"',
        `    private_key_command: "touch ${marker}; printf key"`,
      ].join("\n")
    );

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
    const configPath = path.join(dir, "legion.yaml");
    fs.writeFileSync(
      configPath,
      [
        "project: acme/99",
        "envoy_url: http://127.0.0.1:9020",
        "dispatch_project: ACME",
        "repos:",
        "  - acme/widgets",
        "nats_urls:",
        "  - nats://one:4222",
        "gates:",
        "  design: off",
        "github_apps:",
        "  implement:",
        '    app_id: "1"',
        "    private_key_secret: GH_AGENT_APP_PRIVATE_KEY_B64",
      ].join("\n")
    );
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
    const configPath = path.join(dir, "legion.yaml");
    fs.writeFileSync(
      configPath,
      [
        "project: acme/99",
        "envoy_url: http://127.0.0.1:9020",
        "dispatch_project: ACME",
        "repos:",
        "  - acme/widgets",
        "nats_urls:",
        "  - nats://one:4222",
        "gates:",
        "  design: off",
        "github_apps:",
        "  implement:",
        '    app_id: "1"',
      ].join("\n")
    );

    await expect(cmdCheckConfig(undefined, configPath, env)).rejects.toThrow(
      "github_apps.implement requires exactly one of private_key, private_key_command, or private_key_secret"
    );
  });

  it("resolves Dispatch settings from the injected env, not the process environment", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-check-config-"));
    const configPath = path.join(dir, "legion.yaml");
    fs.writeFileSync(
      configPath,
      [
        "project: acme/99",
        "envoy_url: http://127.0.0.1:9020",
        "dispatch_project: ACME",
        "repos:",
        "  - acme/widgets",
        "nats_urls:",
        "  - nats://one:4222",
        "gates:",
        "  design: off",
      ].join("\n")
    );

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
    const configPath = path.join(dir, "legion.yaml");
    const base = [
      "project: acme/99",
      "envoy_url: http://127.0.0.1:9020",
      "dispatch_project: ACME",
      "repos:",
      "  - acme/widgets",
      "nats_urls:",
      "  - nats://one:4222",
      "gates:",
      "  design: off",
    ];
    fs.writeFileSync(configPath, [...base, "worker_boot_timeout_seconds: 2147484"].join("\n"));

    await expect(cmdCheckConfig(undefined, configPath, env)).rejects.toThrow(
      "worker_boot_timeout_seconds must be at most 2147483"
    );

    // The same parser accepts the bound itself; the registration deadline multiplies the boot
    // timeout by the intervals, so the default 3 intervals would overflow at the bound.
    fs.writeFileSync(
      configPath,
      [
        ...base,
        "worker_boot_timeout_seconds: 2147483",
        "worker_boot_registration_deadline_intervals: 1",
      ].join("\n")
    );
    await cmdCheckConfig(undefined, configPath, env);
  });

  it("resolves relative instructions and state_dir against the --config file's directory, not the cwd", async () => {
    const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "legion-check-config-")));
    const configPath = path.join(dir, "legion.yaml");
    fs.writeFileSync(
      configPath,
      [
        "project: acme/99",
        "envoy_url: http://127.0.0.1:9020",
        "dispatch_project: ACME",
        "repos:",
        "  - acme/widgets",
        "nats_urls:",
        "  - nats://one:4222",
        "gates:",
        "  design: off",
        "state_dir: ./state",
        "instructions: ./ops/deployment.md",
      ].join("\n")
    );
    expect(process.cwd()).not.toBe(dir);

    const config = loadStartConfig(undefined, configPath, env, { resolveSecrets: false });

    expect(config.stateDir).toBe(path.join(dir, "state"));
    expect(config.instructionsPath).toBe(path.join(dir, "ops", "deployment.md"));
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
  const passing = async () => ({
    stdout: "",
    stderr: "LEGION_OMP_AGENTS=available\nLEGION_PLUGIN_LOADED=yes\n",
    exitCode: 0,
  });

  it("refuses to probe without an explicit OMP executable (no PATH fallback)", async () => {
    let ran = false;
    await expect(
      cmdProbeImage(
        undefined,
        deps(async () => {
          ran = true;
          return passing();
        }, {})
      )
    ).rejects.toEqual(
      expect.objectContaining({ message: expect.stringContaining("LEGION_OMP_PATH"), code: 1 })
    );
    expect(ran).toBe(false);
  });

  it("runs the daemon's two boot probes against LEGION_OMP_PATH with no launch prefix", async () => {
    const commands: string[][] = [];
    await cmdProbeImage(
      undefined,
      deps(
        async (command) => {
          commands.push(command);
          return passing();
        },
        { LEGION_OMP_PATH: "/opt/omp/bin/omp" }
      )
    );
    expect(commands).toHaveLength(2);
    expect(commands[0]?.[2]).toStartWith(
      'exec /opt/omp/bin/omp models --no-extensions --extension "$1" --json'
    );
    expect(commands[1]?.[2]).toStartWith('exec /opt/omp/bin/omp models --extension "$1" --json');
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
});

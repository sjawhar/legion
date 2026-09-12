import { describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  cmdCheckConfig,
  cmdGh,
  cmdHandoffComplete,
  loadStartConfig,
  resolveControllerSecret,
} from "../index";

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
          LEGION_GRANT: "grant-123",
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

  it("fails loudly when the worker extension did not inject a grant", async () => {
    await expect(
      cmdGh(["api", "user"], {
        env: {},
        fetch: async () => Response.json({ token: "unused" }),
        spawnGh: async () => 0,
      })
    ).rejects.toEqual(
      expect.objectContaining({
        message: expect.stringContaining("worker extension"),
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
      env: { LEGION_GRANT: "grant-123" },
      fetch: async () =>
        Response.json({ token: "scoped-token", appLogin: "legion-implementer[bot]" }),
      spawnGh: async (args) => {
        spawnArgs = args;
        return 0;
      },
    });

    expect(spawnArgs).toEqual(["pr", "view", "merge-fix"]);
  });
});
describe("legion start --check-config", () => {
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

    await cmdCheckConfig(undefined, configPath);

    expect(fs.existsSync(marker)).toBe(false);
  });

  it("still rejects a github app with neither private_key nor private_key_command", async () => {
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

    await expect(cmdCheckConfig(undefined, configPath)).rejects.toThrow(
      "github_apps.implement requires exactly one of private_key or private_key_command"
    );
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

    const config = loadStartConfig(undefined, configPath, { resolveSecrets: false });

    expect(config.stateDir).toBe(path.join(dir, "state"));
    expect(config.instructionsPath).toBe(path.join(dir, "ops", "deployment.md"));
  });
});

describe("legion handoff complete", () => {
  it("posts the phase-complete request built from LEGION_GRANT", async () => {
    let request: Request | undefined;

    await cmdHandoffComplete("Verified the acceptance criteria end to end.", {
      env: { LEGION_GRANT: "grant-123" },
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

  it("fails loudly when the worker extension did not inject a grant", async () => {
    await expect(
      cmdHandoffComplete("smoke", {
        env: {},
        fetch: async () => Response.json({}),
      })
    ).rejects.toEqual(
      expect.objectContaining({
        message: expect.stringContaining("worker extension"),
        code: 1,
      })
    );
  });

  it("fails loudly when the daemon rejects the request", async () => {
    await expect(
      cmdHandoffComplete("smoke", {
        env: { LEGION_GRANT: "grant-123" },
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
        env: { LEGION_GRANT: "grant-123" },
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

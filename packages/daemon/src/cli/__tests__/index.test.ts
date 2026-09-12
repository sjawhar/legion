import { describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { CommandRunner } from "../../state/fetch";
import {
  cmdCheckConfig,
  cmdGh,
  cmdHandoffComplete,
  cmdProbeImage,
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

  it("sends merge intent to the daemon for a pr merge invocation and runs gh when granted", async () => {
    let request: Request | undefined;
    let spawnArgs: string[] | undefined;
    let childEnvironment: NodeJS.ProcessEnv | undefined;

    await cmdGh(["pr", "merge", "123", "--squash"], {
      env: { LEGION_GRANT: "controller-grant" },
      fetch: async (input, init) => {
        request = new Request(String(input), init);
        return Response.json({ token: "merge-token", appLogin: "legion-implement[bot]" });
      },
      spawnGh: async (args, env) => {
        spawnArgs = args;
        childEnvironment = env;
        return 0;
      },
    });

    expect(new URL(request?.url ?? "").pathname).toBe("/legion/v1/gh-token");
    expect(await request?.json()).toEqual({ grantId: "controller-grant", merge: true });
    expect(spawnArgs).toEqual(["pr", "merge", "123", "--squash"]);
    expect(childEnvironment?.GH_TOKEN).toBe("merge-token");
  });

  it("refuses a pr merge the daemon does not authorise, naming the reason", async () => {
    let spawnCalled = false;

    await expect(
      cmdGh(["pr", "merge", "123", "--squash"], {
        env: { LEGION_GRANT: "worker-grant" },
        fetch: async () =>
          Response.json(
            { error: "Only the controller may merge; publish READY to the controller" },
            { status: 403 }
          ),
        spawnGh: async () => {
          spawnCalled = true;
          return 0;
        },
      })
    ).rejects.toEqual(
      expect.objectContaining({
        message:
          "this grant cannot merge; publish READY to the controller: Only the controller may merge; publish READY to the controller",
        code: 1,
      })
    );
    expect(spawnCalled).toBe(false);
  });

  it("still reports an ordinary redemption failure by status when no merge was intended", async () => {
    await expect(
      cmdGh(["api", "user"], {
        env: { LEGION_GRANT: "expired-grant" },
        fetch: async () => Response.json({ error: "Invalid or expired grant" }, { status: 403 }),
        spawnGh: async () => 0,
      })
    ).rejects.toEqual(
      expect.objectContaining({
        message: "Unable to redeem LEGION_GRANT (403): Invalid or expired grant",
        code: 1,
      })
    );
  });

  it("treats the --repo and REST shapes as merge invocations too: `gh pr --repo <value> merge`, `gh api .../pulls/<n>/merge`", async () => {
    for (const args of [
      ["pr", "--repo", "acme/widgets", "merge", "123"],
      ["api", "-X", "PUT", "repos/acme/widgets/pulls/123/merge"],
    ]) {
      let request: Request | undefined;
      await cmdGh(args, {
        env: { LEGION_GRANT: "controller-grant" },
        fetch: async (input, init) => {
          request = new Request(String(input), init);
          return Response.json({ token: "merge-token", appLogin: "legion-implement[bot]" });
        },
        spawnGh: async () => 0,
      });
      expect(await request?.json()).toEqual({ grantId: "controller-grant", merge: true });
    }
  });

  it("allows a pr subcommand that merely mentions merge in an unrelated argument, without merge intent", async () => {
    let request: Request | undefined;
    let spawnArgs: string[] | undefined;

    await cmdGh(["pr", "view", "merge-fix"], {
      env: { LEGION_GRANT: "grant-123" },
      fetch: async (input, init) => {
        request = new Request(String(input), init);
        return Response.json({ token: "scoped-token", appLogin: "legion-implementer[bot]" });
      },
      spawnGh: async (args) => {
        spawnArgs = args;
        return 0;
      },
    });

    expect(await request?.json()).toEqual({ grantId: "grant-123" });
    expect(spawnArgs).toEqual(["pr", "view", "merge-fix"]);
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

    await expect(cmdCheckConfig(undefined, configPath, env)).rejects.toThrow(
      "github_apps.implement requires exactly one of private_key or private_key_command"
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
});

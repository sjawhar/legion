import { describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { cmdCheckConfig, cmdGh } from "../index";

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
        "nats_urls:",
        "  - nats://one:4222",
        "gates:",
        "  design: off",
        "  merge: human",
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
        "nats_urls:",
        "  - nats://one:4222",
        "gates:",
        "  design: off",
        "  merge: human",
        "github_apps:",
        "  implement:",
        '    app_id: "1"',
      ].join("\n")
    );

    await expect(cmdCheckConfig(undefined, configPath)).rejects.toThrow(
      "github_apps.implement requires exactly one of private_key or private_key_command"
    );
  });
});

import { describe, expect, it, spyOn } from "bun:test";
import {
  execCredentialProvider,
  parseExecConfig,
  type ExecConfig,
} from "../k8s-exec-credential";

const execCfg: ExecConfig = {
  command: "aws",
  args: ["--region", "us-west-2", "eks", "get-token", "--cluster-name", "production"],
  apiVersion: "client.authentication.k8s.io/v1beta1",
};

function credential(token: string, expiresAt: string): string {
  return JSON.stringify({
    apiVersion: execCfg.apiVersion,
    kind: "ExecCredential",
    status: { token, expirationTimestamp: expiresAt },
  });
}

describe("execCredentialProvider", () => {
  it("caches status.token until one minute before expirationTimestamp", async () => {
    let now = Date.parse("2026-09-16T00:00:00Z");
    const calls: string[][] = [];
    const logs: string[] = [];
    const log = spyOn(console, "info").mockImplementation((message: string) => logs.push(message));
    const provider = execCredentialProvider(execCfg, {
      now: () => now,
      run: async (argv) => {
        calls.push(argv);
        return {
          stdout: credential(`t${calls.length}`, "2026-09-16T00:15:00Z"),
          exitCode: 0,
          stderr: "",
        };
      },
    });

    try {
      expect(await provider.token()).toBe("t1");
      now += 13 * 60_000;
      expect(await provider.token()).toBe("t1");
      now += 61_000;
      expect(await provider.token()).toBe("t2");
      expect(calls).toEqual([
        ["aws", "--region", "us-west-2", "eks", "get-token", "--cluster-name", "production"],
        ["aws", "--region", "us-west-2", "eks", "get-token", "--cluster-name", "production"],
      ]);
      expect(logs).toEqual([
        "[legion] kubeconfig exec plugin minted a token (expires 2026-09-16T00:15:00.000Z)",
        "[legion] kubeconfig exec plugin minted a token (expires 2026-09-16T00:15:00.000Z)",
      ]);
    } finally {
      log.mockRestore();
    }
  });

  it("invalidates a cached token before the next request", async () => {
    let calls = 0;
    const provider = execCredentialProvider(execCfg, {
      now: () => 0,
      run: async () => ({
        stdout: credential(`t${++calls}`, "2999-01-01T00:00:00Z"),
        exitCode: 0,
        stderr: "",
      }),
    });

    expect(await provider.token()).toBe("t1");
    provider.invalidate();
    expect(await provider.token()).toBe("t2");
  });

  it("names the plugin and stderr when the command fails", async () => {
    const provider = execCredentialProvider(execCfg, {
      now: () => 0,
      run: async () => ({ stdout: "", exitCode: 255, stderr: "Unable to locate credentials" }),
    });

    await expect(provider.token()).rejects.toThrow(
      "kubeconfig exec plugin `aws --region us-west-2 eks get-token --cluster-name production` exited 255: Unable to locate credentials"
    );
  });

  it("rejects plugin output without a token", async () => {
    const provider = execCredentialProvider(execCfg, {
      now: () => 0,
      run: async () => ({ stdout: "{}", exitCode: 0, stderr: "" }),
    });

    await expect(provider.token()).rejects.toThrow(
      "kubeconfig exec plugin `aws --region us-west-2 eks get-token --cluster-name production` returned no status.token"
    );
  });

  it("parses exec configuration and leaves static-token users alone", () => {
    expect(
      parseExecConfig(
        {
          exec: {
            command: "aws",
            args: ["x"],
            apiVersion: "client.authentication.k8s.io/v1beta1",
            env: [{ name: "AWS_PROFILE", value: "p" }],
          },
        },
        "/k"
      )
    ).toEqual({
      command: "aws",
      args: ["x"],
      apiVersion: "client.authentication.k8s.io/v1beta1",
      env: [{ name: "AWS_PROFILE", value: "p" }],
    });
    expect(parseExecConfig({ token: "abc" }, "/k")).toBeUndefined();
    expect(() => parseExecConfig({ exec: { args: [] } }, "/k")).toThrow(
      "/k: users[].user.exec.command is required"
    );
  });
});

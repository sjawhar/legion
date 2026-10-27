import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { SessionContext, ToolResultEvent } from "@legion/pi-shared/pi-types";
import {
  createToolResultRedactor,
  environmentSecrets,
  ownEnvironment,
  redactResult,
  redactText,
} from "./secret-redaction";

// Fixture credentials: shaped like the real ones, so a test reads like the incident, and random
// enough that nothing else in a fixture contains them by chance.
const ANTHROPIC_KEY = `sk-ant-api03-${"aB3dE5fG7hJ9kL1mN".repeat(5)}-QQ`;
const OPENAI_KEY = `sk-proj-${"Zx8Cv6Bn4Mq2Wr0Ty".repeat(8)}`;

let scratch: string;
beforeAll(async () => {
  scratch = await mkdtemp(path.join(os.tmpdir(), "legion-secret-redaction-"));
});
afterAll(async () => {
  await rm(scratch, { recursive: true, force: true });
});

/** The text a tool returns for `/proc/<pid>/environ`: the pairs back to back, the NUL bytes
 * between them already stripped, as the incident's `read` result showed them. */
function environDump(entries: readonly (readonly [string, string])[]): string {
  return entries.map(([name, value]) => `${name}=${value}`).join("");
}

describe("environmentSecrets and redactText", () => {
  test("the incident: provider keys in a NUL-stripped environ dump are redacted and the names around them kept", () => {
    const environment = [
      ["PATH", "/usr/local/bin:/usr/bin:/bin"],
      ["PI_CONFIG_FILES", "/var/run/legion/state/podsafety-turnscope-overlay.yml"],
      ["ANTHROPIC_API_KEY", ANTHROPIC_KEY],
      ["OPENAI_API_KEY", OPENAI_KEY],
    ] as const;
    const redacted = redactText(environDump(environment), environmentSecrets(environment));
    expect(redacted).not.toContain(ANTHROPIC_KEY);
    expect(redacted).not.toContain(OPENAI_KEY);
    expect(redacted).toContain(
      "ANTHROPIC_API_KEY=[redacted by Legion: $ANTHROPIC_API_KEY]OPENAI_API_KEY=[redacted by Legion: $OPENAI_API_KEY]"
    );
    expect(redacted).toStartWith("PATH=/usr/local/bin:/usr/bin:/bin");
  });

  test("fails closed: a credential under a name no rule knows is redacted", () => {
    // Not credential-shaped and not one of Legion's own: an operator's licence key, an image's
    // token, a variable nobody thought of. Only its length says it may be a secret.
    const licence = "WGT-9f8e7d6c5b4a39281706f5e4d3c2b1a0";
    const environment = [["WIDGET_LICENCE", licence]] as const;
    expect(redactText(`licence is ${licence}.`, environmentSecrets(environment))).toBe(
      "licence is [redacted by Legion: $WIDGET_LICENCE]."
    );
  });

  test("a short credential under a credential-shaped name is redacted", () => {
    const environment = [["DB_PASSWORD", "hunter22"]] as const;
    expect(redactText("password=hunter22", environmentSecrets(environment))).toBe(
      "password=[redacted by Legion: $DB_PASSWORD]"
    );
  });

  test("the token a *_FILE pointer names is redacted wherever its contents appear", async () => {
    const tokenFile = path.join(scratch, "dispatch-token");
    await writeFile(tokenFile, "dsp_0123456789abcdefghijklmnop\n", { mode: 0o600 });
    const environment = [["DISPATCH_TOKEN_FILE", tokenFile]] as const;
    const secrets = environmentSecrets(environment);
    expect(redactText("Authorization: Bearer dsp_0123456789abcdefghijklmnop", secrets)).toBe(
      "Authorization: Bearer [redacted by Legion: contents of $DISPATCH_TOKEN_FILE]"
    );
    // The pointer itself is a path, which stays readable.
    expect(redactText(`token file ${tokenFile}`, secrets)).toBe(`token file ${tokenFile}`);
  });

  test("a URL's password is redacted and the rest of the URL kept, even under one of Legion's own names", () => {
    const environment = [
      ["HTTPS_PROXY", "http://agent:s3cretPassw0rd@proxy.internal.example:3128"],
    ] as const;
    expect(
      redactText(
        "proxy: http://agent:s3cretPassw0rd@proxy.internal.example:3128",
        environmentSecrets(environment)
      )
    ).toBe(
      "proxy: http://agent:[redacted by Legion: password in $HTTPS_PROXY]@proxy.internal.example:3128"
    );
  });

  test("each long line of a multi-line value is redacted, so a value printed in pieces loses every piece", () => {
    const pem = [
      "-----BEGIN PRIVATE KEY-----",
      "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7",
      "VJTUt9Us8cKjMzEfYyjiWA4R4/M2bS1GB4t7NXp98C3SC6dVMvDu",
      "-----END PRIVATE KEY-----",
    ].join("\n");
    const environment = [["APP_SIGNING", pem]] as const;
    const redacted = redactText(
      "line 2: VJTUt9Us8cKjMzEfYyjiWA4R4/M2bS1GB4t7NXp98C3SC6dVMvDu",
      environmentSecrets(environment)
    );
    expect(redacted).toBe("line 2: [redacted by Legion: $APP_SIGNING]");
  });

  test("a longer secret holding a shorter one is replaced whole", () => {
    const environment = [
      ["SHORT_TOKEN", "abcdefgh12345678"],
      ["LONG_TOKEN", "abcdefgh12345678-and-more-characters"],
    ] as const;
    expect(
      redactText("x abcdefgh12345678-and-more-characters y", environmentSecrets(environment))
    ).toBe("x [redacted by Legion: $LONG_TOKEN] y");
  });

  test("Legion's own variables, paths and short values stay readable", () => {
    const environment = [
      ["LEGION_WORKSPACE", scratch],
      ["PATH", `${scratch}:/usr/bin:/bin`],
      ["LEGION_ROLE", "implementer"],
      ["LEGION_DAEMON_URL", "http://legion-daemon.legion.svc.cluster.local:8090"],
      ["GIT_AUTHOR_EMAIL", "343353655+legion-implementer[bot]@users.noreply.github.com"],
      ["PI_SHELL_PREFIX", "set +euo pipefail; PATH=/opt/legion/bin:$PATH"],
      ["DO_NOT_TRACK", "1"],
      ["NODE_ENV", "production"],
    ] as const;
    const text = environDump(environment);
    expect(redactText(text, environmentSecrets(environment))).toBe(text);
  });

  // The limits of the mechanism, held here so they are known rather than assumed. Each is a
  // credential that reaches a tool result unredacted.
  describe("what it does not catch", () => {
    test("a credential this process's environment does not hold passes through", () => {
      // A GitHub token read from a file the environment does not point at by a *_FILE variable
      // (the gh bundle's hosts.yml), printed by a command (`gh auth token`), or held in another
      // process's environment: only this process's own environment is the source of secrets.
      const elsewhere = `ghs_${"R2d2C3po".repeat(4)}abcd`;
      const environment = [["ANTHROPIC_API_KEY", ANTHROPIC_KEY]] as const;
      const text = `oauth_token: ${elsewhere}`;
      expect(redactText(text, environmentSecrets(environment))).toBe(text);
    });

    test("a credential of fewer than 16 characters under a name no rule knows passes through", () => {
      const environment = [["WIDGET_PIN", "pin-12345678"]] as const;
      expect(redactText("pin is pin-12345678", environmentSecrets(environment))).toBe(
        "pin is pin-12345678"
      );
    });

    test("a credential transformed on its way out (encoded, sliced) passes through", () => {
      const environment = [["ANTHROPIC_API_KEY", ANTHROPIC_KEY]] as const;
      const encoded = Buffer.from(ANTHROPIC_KEY).toString("base64");
      const secrets = environmentSecrets(environment);
      expect(redactText(encoded, secrets)).toBe(encoded);
      expect(redactText(ANTHROPIC_KEY.slice(0, 40), secrets)).toBe(ANTHROPIC_KEY.slice(0, 40));
    });
  });
});

describe("ownEnvironment", () => {
  test("holds a value deleted from process.env since start, which /proc/self/environ still shows", () => {
    const initial = `PATH=/usr/bin\0ANTHROPIC_API_KEY=${ANTHROPIC_KEY}\0`;
    const entries = ownEnvironment({ PATH: "/usr/bin", LATER: "set-after-start-value" }, initial);
    expect(entries).toContainEqual(["ANTHROPIC_API_KEY", ANTHROPIC_KEY]);
    expect(entries).toContainEqual(["LATER", "set-after-start-value"]);
    expect(redactText(ANTHROPIC_KEY, environmentSecrets(entries))).toBe(
      "[redacted by Legion: $ANTHROPIC_API_KEY]"
    );
  });

  test("is process.env alone where /proc/self/environ cannot be read", () => {
    expect(ownEnvironment({ A_VARIABLE: "a-value" }, undefined)).toEqual([
      ["A_VARIABLE", "a-value"],
    ]);
  });
});

describe("redactResult", () => {
  const secrets = environmentSecrets([["ANTHROPIC_API_KEY", ANTHROPIC_KEY]]);

  test("redacts every text block and every string in details, at any depth, and keeps images", () => {
    const image = { type: "image" as const, data: "aGVsbG8=", mimeType: "image/png" };
    const result = redactResult(
      {
        content: [{ type: "text", text: `ANTHROPIC_API_KEY=${ANTHROPIC_KEY}` }, image],
        details: {
          totalLines: 1,
          displayContent: { text: `A=1\u0000ANTHROPIC_API_KEY=${ANTHROPIC_KEY}\u0000B=2` },
          lines: [ANTHROPIC_KEY, 3],
        },
      },
      secrets
    );
    expect(result).toBeDefined();
    expect(JSON.stringify(result)).not.toContain(ANTHROPIC_KEY);
    expect(result?.content).toEqual([
      { type: "text", text: "ANTHROPIC_API_KEY=[redacted by Legion: $ANTHROPIC_API_KEY]" },
      image,
    ]);
    expect(result?.details).toEqual({
      totalLines: 1,
      displayContent: {
        text: "A=1\u0000ANTHROPIC_API_KEY=[redacted by Legion: $ANTHROPIC_API_KEY]\u0000B=2",
      },
      lines: ["[redacted by Legion: $ANTHROPIC_API_KEY]", 3],
    });
  });

  test("answers nothing for a result that holds no secret", () => {
    expect(
      redactResult({ content: [{ type: "text", text: "clean" }], details: { a: "b" } }, secrets)
    ).toBeUndefined();
  });
});

describe("createToolResultRedactor", () => {
  const legionEnvironment = [
    ["LEGION_ROLE", "implementer"],
    ["LEGION_TREE", "ACME-1"],
    ["LEGION_ISSUE", "ACME-2"],
    ["ANTHROPIC_API_KEY", ANTHROPIC_KEY],
  ] as const;

  function context(artifacts: Readonly<Record<string, string>> = {}): SessionContext {
    return {
      sessionManager: {
        getArtifactPath: async (id: string) => artifacts[id] ?? null,
      },
    } as unknown as SessionContext;
  }

  function event(text: string, details: unknown = {}): ToolResultEvent {
    return {
      toolName: "read",
      toolCallId: "call-1",
      input: { path: "/proc/self/environ:raw" },
      content: [{ type: "text", text }],
      details,
      isError: false,
    };
  }

  test("redacts a Legion session's tool result", async () => {
    const redact = createToolResultRedactor({ environment: () => legionEnvironment });
    const result = await redact(event(`ANTHROPIC_API_KEY=${ANTHROPIC_KEY}`), context());
    expect(result?.content).toEqual([
      { type: "text", text: "ANTHROPIC_API_KEY=[redacted by Legion: $ANTHROPIC_API_KEY]" },
    ]);
  });

  test("leaves a session with no Legion environment as it is", async () => {
    const redact = createToolResultRedactor({
      environment: () => [["ANTHROPIC_API_KEY", ANTHROPIC_KEY]],
    });
    expect(await redact(event(`key ${ANTHROPIC_KEY}`), context())).toBeUndefined();
  });

  test("rewrites the artifact a large result spilled its full text to", async () => {
    const artifact = path.join(scratch, "7.bash.log");
    await writeFile(artifact, `line\nANTHROPIC_API_KEY=${ANTHROPIC_KEY}\nline\n`);
    const redact = createToolResultRedactor({ environment: () => legionEnvironment });
    await redact(
      event("[output truncated; full output: artifact://7]", {
        meta: { truncation: { artifactId: "7" } },
      }),
      context({ "7": artifact })
    );
    expect(await readFile(artifact, "utf8")).toBe(
      "line\nANTHROPIC_API_KEY=[redacted by Legion: $ANTHROPIC_API_KEY]\nline\n"
    );
  });

  test("withholds the whole result when redaction fails, rather than letting it through", async () => {
    const redact = createToolResultRedactor({
      environment: () => {
        throw new Error("environment unreadable");
      },
    });
    const result = await redact(event(`ANTHROPIC_API_KEY=${ANTHROPIC_KEY}`), context());
    expect(result?.isError).toBe(true);
    expect(JSON.stringify(result)).not.toContain(ANTHROPIC_KEY);
    expect(result?.content).toEqual([
      {
        type: "text",
        text: "Legion withheld this tool result: redacting credentials from it failed (environment unreadable)",
      },
    ]);
  });
});

import { describe, expect, test } from "bun:test";
import { resolve } from "node:path";

const output = resolve(import.meta.dir, "../../envoy/internal/contracts/generated.go");
const generator = resolve(import.meta.dir, "gen-go.ts");
const repoRoot = resolve(import.meta.dir, "../../..");

describe("gen-go", () => {
  // Spawns the generator (which shells out to gofmt) and a `go test` that compiles
  // the contracts package from cold on a CI runner; Bun's 5 s default cannot cover it.
  test("emits the envelope sender struct", async () => {
    const process = Bun.spawn(["bun", generator], {
      cwd: repoRoot,
      stderr: "pipe",
      stdout: "pipe",
    });
    const stderr = await new Response(process.stderr).text();
    const exitCode = await process.exited;

    expect(stderr).toBe("");
    expect(exitCode).toBe(0);

    const source = await Bun.file(output).text();
    expect(source).toContain(`type EnvelopeSender struct {
\tSessionID string   \`json:"session_id"\`
\tMachine   string   \`json:"machine,omitempty"\`
\tCwd       string   \`json:"cwd,omitempty"\`
\tTitle     string   \`json:"title,omitempty"\`
\tRoles     []string \`json:"roles,omitempty"\`
}`);

    const goTest = Bun.spawn(["go", "test", "./internal/contracts/...", "-run", "^TestEnvelopeSignalQualityFieldsRoundTrip$"], {
      cwd: resolve(repoRoot, "packages/envoy"),
      stderr: "pipe",
      stdout: "pipe",
    });
    const goStderr = await new Response(goTest.stderr).text();

    expect(goStderr).toBe("");
    expect(await goTest.exited).toBe(0);
  }, 120_000);
});

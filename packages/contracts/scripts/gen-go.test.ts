import { describe, expect, test } from "bun:test";
import { resolve } from "node:path";

const output = resolve(import.meta.dir, "../../envoy/internal/contracts/generated.go");
const questionSchema = resolve(import.meta.dir, "../schemas/dispatch-question.schema.json");
const generator = resolve(import.meta.dir, "gen-go.ts");
const repoRoot = resolve(import.meta.dir, "../../..");

describe("gen-go", () => {
  // The generator shells out to gofmt, so Bun's 5 s default cannot cover a cold CI runner.
  test("emits the envelope Go contract without a question schema", async () => {
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
    expect(await Bun.file(questionSchema).exists()).toBe(false);
  }, 120_000);
});

import { describe, expect, test } from "bun:test";
import { relative, resolve } from "node:path";

const output = resolve(import.meta.dir, "../../envoy/internal/contracts/generated.go");
const daemonOutput = resolve(import.meta.dir, "../../daemon/internal/dispatch/generated.go");
const questionSchema = resolve(import.meta.dir, "../schemas/dispatch-question.schema.json");
const generator = resolve(import.meta.dir, "gen-go.ts");
const repoRoot = resolve(import.meta.dir, "../../..");

describe("gen-go", () => {
  // The generator shells out to gofmt, so Bun's 5 s default cannot cover a cold CI runner.
  // `--check` compares a fresh render with the committed file and never writes it, so a constant
  // changed in TypeScript without regenerating fails here instead of being rewritten in place.
  test("the committed Go contract is what the generator emits, without a question schema", async () => {
    const process = Bun.spawn(["bun", generator, "--check"], {
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

  // Each file the generator writes is checked, so a hand-edit of any of them fails CI naming the
  // file it found stale rather than passing because a sibling file still matches.
  for (const path of [output, daemonOutput]) {
    test(`a hand-edit of ${relative(repoRoot, path)} makes --check fail, naming it`, async () => {
      const before = await Bun.file(path).text();
      try {
        await Bun.write(path, `${before}\n// hand-edited\n`);
        const process = Bun.spawn(["bun", generator, "--check"], {
          cwd: repoRoot,
          stderr: "pipe",
          stdout: "pipe",
        });
        const stderr = await new Response(process.stderr).text();
        const exitCode = await process.exited;

        expect(exitCode).not.toBe(0);
        expect(stderr).toContain(`${path} is stale`);
      } finally {
        await Bun.write(path, before);
      }
    }, 120_000);
  }
});

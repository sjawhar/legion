import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";

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
  // file it found stale rather than passing because a sibling file still matches. The hand-edit
  // lands on a copy under GEN_GO_CHECK_ROOT, never on the tracked file this workspace shares.
  for (const stale of [output, daemonOutput]) {
    test(`a hand-edit of ${relative(repoRoot, stale)} makes --check fail, naming it`, async () => {
      const root = mkdtempSync(join(tmpdir(), "gen-go-check-"));
      const staleCopy = resolve(root, relative(repoRoot, stale));
      try {
        for (const source of [output, daemonOutput]) {
          const copy = resolve(root, relative(repoRoot, source));
          mkdirSync(dirname(copy), { recursive: true });
          await Bun.write(copy, await Bun.file(source).text());
        }
        await Bun.write(staleCopy, `${await Bun.file(stale).text()}\n// hand-edited\n`);

        const spawned = Bun.spawn(["bun", generator, "--check"], {
          cwd: repoRoot,
          env: { ...process.env, GEN_GO_CHECK_ROOT: root },
          stderr: "pipe",
          stdout: "pipe",
        });
        const stderr = await new Response(spawned.stderr).text();
        const exitCode = await spawned.exited;

        expect(exitCode).not.toBe(0);
        expect(stderr).toContain(`${staleCopy} is stale`);
      } finally {
        rmSync(root, { force: true, recursive: true });
      }
    }, 120_000);
  }
});

import { afterEach, describe, expect, it } from "bun:test";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { exportJjSessionAttribution, sessionIdFromTranscript } from "./jj-attribution";

const ROOT_TRANSCRIPT =
  "/sessions/-legion-default/2026-08-30T03-47-40-329Z_01a050c7-b4a9-7000-a539-240e20271441.jsonl";
const ROOT_ID = "01a050c7-b4a9-7000-a539-240e20271441";

describe("sessionIdFromTranscript", () => {
  it("extracts the resumable id from a top-level transcript", () => {
    expect(sessionIdFromTranscript(ROOT_TRANSCRIPT)).toBe(ROOT_ID);
  });

  it("rejects subagent transcripts named after their agent", () => {
    expect(
      sessionIdFromTranscript("/sessions/-x/2026-08-30T03-47-40-329Z_abc/EnvProbe.jsonl")
    ).toBeUndefined();
  });

  it("rejects timestamped basenames whose id is not a UUID", () => {
    expect(
      sessionIdFromTranscript("/sessions/-x/2026-08-30T03-47-40-329Z_bad'id.jsonl")
    ).toBeUndefined();
  });
});

describe("exportJjSessionAttribution", () => {
  const dirs: string[] = [];
  async function stateDir(): Promise<string> {
    const dir = await mkdtemp(path.join(tmpdir(), "jj-attribution-"));
    dirs.push(dir);
    return dir;
  }
  afterEach(async () => {
    await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
  });

  it("writes the trailer overlay and chains it after the resolved user config", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_TRANSCRIPT, dir, env, async () => "/u/a.toml:/u/b.toml");

    const overlay = path.join(dir, `omp-attribution-${ROOT_ID}.toml`);
    expect(env.JJ_CONFIG).toBe(`/u/a.toml:/u/b.toml:${overlay}`);
    expect(await readFile(overlay, "utf8")).toBe(
      `[templates]\ncommit_trailers = '"Omp-Session: ${ROOT_ID}"'\n`
    );
  });

  it("strips stale attribution overlays from an inherited JJ_CONFIG, keeping user layers", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {
      JJ_CONFIG: `/u/base.toml:/home/x/.cache/omp/jj/omp-attribution-${ROOT_ID.replace("b4a9", "dead")}.toml`,
    };
    await exportJjSessionAttribution(ROOT_TRANSCRIPT, dir, env, async () => {
      throw new Error("must not resolve");
    });
    expect(env.JJ_CONFIG).toBe(`/u/base.toml:${path.join(dir, `omp-attribution-${ROOT_ID}.toml`)}`);
  });

  it("re-resolves the base when the inherited chain was only stale overlays", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = { JJ_CONFIG: "/tmp/omp-attribution-old.toml" };
    await exportJjSessionAttribution(ROOT_TRANSCRIPT, dir, env, async () => "/u/base.toml");
    expect(env.JJ_CONFIG).toBe(`/u/base.toml:${path.join(dir, `omp-attribution-${ROOT_ID}.toml`)}`);
  });

  it("is idempotent when the overlay is already chained", async () => {
    const dir = await stateDir();
    const overlay = path.join(dir, `omp-attribution-${ROOT_ID}.toml`);
    const env: NodeJS.ProcessEnv = { JJ_CONFIG: `/u/base.toml:${overlay}` };
    await exportJjSessionAttribution(ROOT_TRANSCRIPT, dir, env, async () => "/u/base.toml");
    expect(env.JJ_CONFIG).toBe(`/u/base.toml:${overlay}`);
  });

  it("publishes the clean base when the overlay cannot be written", async () => {
    const dir = await stateDir();
    const blocked = path.join(dir, "not-a-directory");
    await Bun.write(blocked, "occupied");
    const env: NodeJS.ProcessEnv = { JJ_CONFIG: "/u/base.toml:/tmp/omp-attribution-old.toml" };
    await exportJjSessionAttribution(ROOT_TRANSCRIPT, path.join(blocked, "nested"), env);
    expect(env.JJ_CONFIG).toBe("/u/base.toml"); // stale overlay gone, no new one
  });

  it("leaves the environment untouched when jj is unavailable", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(ROOT_TRANSCRIPT, dir, env, async () => undefined);
    expect(env.JJ_CONFIG).toBeUndefined();
  });

  it("leaves the environment untouched for subagent transcripts", async () => {
    const dir = await stateDir();
    const env: NodeJS.ProcessEnv = {};
    await exportJjSessionAttribution(`${path.dirname(ROOT_TRANSCRIPT)}/x_1/Worker.jsonl`, dir, env);
    expect(env.JJ_CONFIG).toBeUndefined();
  });
});

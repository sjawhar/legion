import { afterAll, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { DAEMON_MODULE, RIG_GH_TOKEN, workerPane } from "./daemon-pane";

// The run directory: the daemon's pane environment makes the pane's home and the claim's gh
// directory under its state directory.
const rig = mkdtempSync(path.join(tmpdir(), "skill-scenarios-pane-"));
afterAll(() => {
  rmSync(rig, { recursive: true, force: true });
});

// The pane's gh, git and jj are whatever its PATH resolves: the daemon pins none, so the inherited
// PATH needs no tool on it for the pane to be built.
const inherited = {
  ANTHROPIC_API_KEY: "personal-key-must-not-reach-worker",
  GEMINI_API_KEY: "gemini-key",
  OPENAI_API_KEY: "openai-key",
  LEGION_GRANT: "outer-grant",
  DISPATCH_TOKEN: "outer-dispatch-token",
  GH_TOKEN: "outer-github-token",
  GH_CONFIG_DIR: "/outer/gh",
  PATH: "/outer/bin:/usr/bin",
};

// The pane environment comes from the daemon's own functions, compiled and run by `go run`, which
// can take minutes on a cold build cache.
test("strips an inherited Anthropic key and GitHub token, and gives the pane the claim's gh files and no grant file", () => {
  const workerEnv = workerPane(
    { rig, port: 13399, profile: "rig-profile" },
    inherited,
    { project: "skillsrig", issue: "LWEVAL-1", role: "tester" },
    { daemonModule: DAEMON_MODULE }
  ).env;

  expect(workerEnv).not.toHaveProperty("ANTHROPIC_API_KEY");
  expect(workerEnv).not.toHaveProperty("LEGION_GRANT");
  expect(workerEnv).not.toHaveProperty("DISPATCH_TOKEN");
  // The daemon names no grant file on a pane: the `legion` tool mints its grants in-process, and
  // no agent runs `legion` from bash (LEGION-631).
  expect(workerEnv).not.toHaveProperty("LEGION_GRANT_FILE");
  expect(workerEnv).not.toHaveProperty("LEGION_GH_PATH");
  expect(workerEnv).not.toHaveProperty("LEGION_GIT_PATH");
  expect(workerEnv).not.toHaveProperty("LEGION_JJ_PATH");
  const ghConfigDir = `${rig}/state/secrets/legion-skillsrig-lweval-1-tester-gh`;
  expect(workerEnv).toMatchObject({
    GEMINI_API_KEY: "gemini-key",
    OPENAI_API_KEY: "openai-key",
    PATH: `${rig}/state/bin:/outer/bin:/usr/bin`,
    // The caller's own token and directory never reach the pane: the daemon's pane sets the three
    // token variables empty beside the claim's directory, so gh reads the file and nothing else.
    GH_CONFIG_DIR: ghConfigDir,
    GH_TOKEN: "",
    GITHUB_TOKEN: "",
    GH_HOST: "",
    LEGION_ISSUE: "LWEVAL-1",
    LEGION_ROLE: "tester",
    XDG_CONFIG_HOME: `${rig}/state/home/.config`,
  });
  // The claim's gh files hold the rig's token in gh's own migrated shape, readable by the pane
  // alone, as the daemon's tmux runtime writes them.
  expect(statSync(ghConfigDir).mode & 0o777).toBe(0o700);
  expect(statSync(path.join(ghConfigDir, "hosts.yml")).mode & 0o777).toBe(0o600);
  expect(readFileSync(path.join(ghConfigDir, "hosts.yml"), "utf8")).toContain(
    `oauth_token: ${RIG_GH_TOKEN}`
  );
  expect(readFileSync(path.join(ghConfigDir, "config.yml"), "utf8")).toBe('version: "1"\n');
}, 600_000);

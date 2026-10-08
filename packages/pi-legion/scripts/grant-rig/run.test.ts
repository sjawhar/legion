import { afterAll, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import {
  invokesLegion,
  launchArgv,
  probeShowsPaneEnvironment,
  RIG_GH_TOKEN,
  workerPane,
} from "./run";

// The rig's state directory: the daemon's pane environment makes the pane's home and the claim's
// gh directory under it.
const rig = mkdtempSync(path.join(tmpdir(), "grant-rig-"));
const launch = { rig, port: 13399, omp: "/opt/omp/bin/omp", profile: "rig-profile" };
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
test("strips an inherited Anthropic key and GitHub token, and gives the pane the claim's gh files, in both worker launch modes", () => {
  const workerEnv = workerPane(launch, inherited).env;

  expect(workerEnv).not.toHaveProperty("ANTHROPIC_API_KEY");
  expect(workerEnv).not.toHaveProperty("LEGION_GRANT");
  expect(workerEnv).not.toHaveProperty("DISPATCH_TOKEN");
  expect(workerEnv).not.toHaveProperty("LEGION_GH_PATH");
  expect(workerEnv).not.toHaveProperty("LEGION_GIT_PATH");
  expect(workerEnv).not.toHaveProperty("LEGION_JJ_PATH");
  const ghConfigDir = `${rig}/state/secrets/legion-l12rig-rig-1-implementer-gh`;
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
    LEGION_GRANT_FILE: `${rig}/state/secrets/legion-l12rig-rig-1-implementer-grant`,
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
  // Verdict G reads exactly this environment back from the pane's probe.
  expect(
    probeShowsPaneEnvironment(
      `GH_CONFIG_DIR=${ghConfigDir} GH_TOKEN= GITHUB_TOKEN= GH_HOST=\nPATH=${workerEnv.PATH}`,
      ghConfigDir,
      `${rig}/state/bin`
    )
  ).toBe(true);
  for (const [mode, useSecrets] of [
    ["rpc", true],
    ["tui", false],
  ] as const) {
    // `secrets` reads the caller's secretsd configuration; Oh My Pi gets the pane's XDG_CONFIG_HOME.
    expect(launchArgv({ ...launch, useSecrets }, mode, workerEnv)).toEqual(
      useSecrets
        ? [
            ...[
              "env",
              "-u",
              "XDG_CONFIG_HOME",
              "secrets",
              "GEMINI_API_KEY",
              "OPENAI_API_KEY",
              "--",
            ],
            ...[
              "env",
              `XDG_CONFIG_HOME=${rig}/state/home/.config`,
              "/opt/omp/bin/omp",
              "--mode",
              "rpc",
            ],
          ]
        : ["/opt/omp/bin/omp"]
    );
  }
}, 600_000);

test("pairs grant mints with the bash calls that invoke legion, as the extension's hook does", () => {
  // The prompt's own commands: every record-grant step invokes `legion state` first, the gh and
  // probe steps invoke no `legion`, and a word that only contains `legion` is not an invocation.
  expect(invokesLegion("legion state --json >/dev/null; record-grant; echo step-1")).toBe(true);
  expect(invokesLegion("legion state; echo exit=$?")).toBe(true);
  expect(invokesLegion("cd ws && /opt/legion/bin/legion push | cat")).toBe(true);
  expect(invokesLegion("gh --version")).toBe(false);
  expect(invokesLegion("gh auth token; echo exit=$?")).toBe(false);
  expect(invokesLegion('printf \'GH_CONFIG_DIR=%s\\n\' "$GH_CONFIG_DIR"; echo "PATH=$PATH"')).toBe(
    false
  );
  expect(invokesLegion("echo legion-worker")).toBe(false);
  // Verdict G refuses a probe whose GH_CONFIG_DIR is another directory, whose token variables are
  // set, whose PATH does not lead with the launcher directory, or whose PATH still carries a
  // worker-bin entry.
  const launcher = "/rig/state/bin";
  const gh = "/rig/state/secrets/legion-l12rig-rig-1-implementer-gh";
  const probe = (environment: string, pathValue: string) =>
    probeShowsPaneEnvironment(`${environment}\nPATH=${pathValue}`, gh, launcher);
  const clean = `GH_CONFIG_DIR=${gh} GH_TOKEN= GITHUB_TOKEN= GH_HOST=`;
  expect(probe(clean, `${launcher}:/usr/bin`)).toBe(true);
  expect(probe("GH_CONFIG_DIR= GH_TOKEN= GITHUB_TOKEN= GH_HOST=", `${launcher}:/usr/bin`)).toBe(
    false
  );
  expect(
    probe(`GH_CONFIG_DIR=${gh} GH_TOKEN=x GITHUB_TOKEN= GH_HOST=`, `${launcher}:/usr/bin`)
  ).toBe(false);
  expect(probe(clean, `/usr/bin:${launcher}`)).toBe(false);
  expect(probe(clean, `${launcher}:/usr/bin:${launcher}`)).toBe(false);
  expect(probe(clean, `${launcher}:/rig/state/worker-bin:/usr/bin`)).toBe(false);
});

import { afterAll, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { DAEMON_MODULE, RIG_GH_TOKEN, workerPane } from "./daemon-pane";

// The run directory: the daemon's pane environment makes the pane's home and the claim's gh
// directory under its state directory.
const rig = mkdtempSync(path.join(tmpdir(), "skill-scenarios-pane-"));
afterAll(() => {
  rmSync(rig, { recursive: true, force: true });
});

// The caller's home, where the profile's agent directory and its envoy.json live: a scratch one, so
// no envoy.json of the person running the test is read.
const home = path.join(rig, "caller-home");

// The pane's git and jj are whatever its PATH resolves, and its gh the launcher directory's
// wrapper over the real gh the daemon resolves on its PATH at boot: the inherited PATH holds a
// stand-in gh, the one tool the daemon needs on it for the pane to be built. It reads as a
// compiled program (no `#!` line), since the daemon refuses a script as the first gh on PATH.
const outerBin = path.join(rig, "outer-bin");
mkdirSync(outerBin);
writeFileSync(path.join(outerBin, "gh"), "\x7fELF\x02\x01\x01\x00", { mode: 0o700 });
const scriptBin = path.join(rig, "script-bin");
mkdirSync(scriptBin);
writeFileSync(path.join(scriptBin, "gh"), "#!/bin/sh\nexit 0\n", { mode: 0o700 });
const inherited = {
  HOME: home,
  ANTHROPIC_API_KEY: "personal-key-must-not-reach-worker",
  GEMINI_API_KEY: "gemini-key",
  OPENAI_API_KEY: "openai-key",
  LEGION_GRANT: "outer-grant",
  DISPATCH_TOKEN: "outer-dispatch-token",
  GH_TOKEN: "outer-github-token",
  GH_CONFIG_DIR: "/outer/gh",
  LEGION_GH_PATH: "/outer/gh-binary",
  PATH: `${outerBin}:/usr/bin`,
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
  expect(workerEnv).not.toHaveProperty("LEGION_GIT_PATH");
  expect(workerEnv).not.toHaveProperty("LEGION_JJ_PATH");
  const ghRoot = `${rig}/state/secrets/legion-skillsrig-lweval-1-tester-gh`;
  const ghConfigDir = `${ghRoot}/skills-rig`;
  expect(workerEnv).toMatchObject({
    GEMINI_API_KEY: "gemini-key",
    OPENAI_API_KEY: "openai-key",
    PATH: `${rig}/state/bin:${outerBin}:/usr/bin`,
    // The caller's own token and directory never reach the pane: the daemon's pane sets the three
    // token variables empty beside the claim's gh bundle, so gh reads the files and nothing else,
    // and its wrapper runs the real gh the daemon found on its PATH, not the caller's.
    GH_CONFIG_DIR: ghConfigDir,
    LEGION_GH_OWNERS_DIR: ghRoot,
    LEGION_GH_PATH: path.join(outerBin, "gh"),
    GH_TOKEN: "",
    GITHUB_TOKEN: "",
    GH_HOST: "",
    LEGION_ISSUE: "LWEVAL-1",
    LEGION_ROLE: "tester",
    XDG_CONFIG_HOME: `${rig}/state/home/.config`,
  });
  // The claim's gh bundle holds the rig's token in gh's own migrated shape, readable by the pane
  // alone, as the daemon's tmux runtime writes it.
  expect(statSync(ghRoot).mode & 0o777).toBe(0o700);
  expect(statSync(ghConfigDir).mode & 0o777).toBe(0o700);
  expect(statSync(path.join(ghConfigDir, "hosts.yml")).mode & 0o777).toBe(0o600);
  expect(readFileSync(path.join(ghConfigDir, "hosts.yml"), "utf8")).toContain(
    `oauth_token: ${RIG_GH_TOKEN}`
  );
  expect(readFileSync(path.join(ghConfigDir, "config.yml"), "utf8")).toBe('version: "1"\n');
}, 600_000);

// The pane's own Envoy extension reads the profile's envoy.json, so the daemon is asked to name the
// listener and NATS servers written there.
test("names the listener and NATS servers the profile's envoy.json writes", () => {
  const agent = path.join(home, ".omp", "profiles", "rig-envoy-profile", "agent");
  mkdirSync(agent, { recursive: true });
  writeFileSync(
    path.join(agent, "envoy.json"),
    JSON.stringify({
      envoy: {
        enabled: true,
        url: "http://rig-listener.internal.example:9020",
        natsUrls: ["nats://rig-nats.internal.example:4222"],
      },
    })
  );
  const workerEnv = workerPane(
    { rig, port: 13399, profile: "rig-envoy-profile" },
    inherited,
    { project: "skillsrig", issue: "LWEVAL-1", role: "tester" },
    { daemonModule: DAEMON_MODULE }
  ).env;

  expect(workerEnv).toMatchObject({
    OMP_PROFILE: "rig-envoy-profile",
    PI_CODING_AGENT_DIR: agent,
    ENVOY_URL: "http://rig-listener.internal.example:9020",
    ENVOY_NATS_URL: "nats://rig-nats.internal.example:4222",
  });
}, 600_000);

// A seed the profile's envoy.json pairs with other servers than ENVOY_NATS_URL leaves Envoy on with
// inbound messages off, which the pane's own extension would meet; the rig refuses it, as it refuses
// an Envoy that is off with an error, rather than ask the daemon for a pane with no NATS server.
test("refuses a profile envoy.json whose NATS seed Envoy refuses", () => {
  const agent = path.join(home, ".omp", "profiles", "rig-seed-profile", "agent");
  mkdirSync(agent, { recursive: true });
  const file = path.join(agent, "envoy.json");
  writeFileSync(
    file,
    JSON.stringify({
      envoy: {
        url: "http://rig-listener.internal.example:9020",
        natsUrls: ["nats://rig-nats.internal.example:4222"],
        natsSeed: "rig-seed-placeholder",
      },
    })
  );

  expect(() =>
    workerPane(
      { rig, port: 13399, profile: "rig-seed-profile" },
      { ...inherited, ENVOY_NATS_URL: "nats://other-nats.internal.example:4222" },
      { project: "skillsrig", issue: "LWEVAL-1", role: "tester" },
      { daemonModule: DAEMON_MODULE }
    )
  ).toThrow(
    `envoy: ENVOY_NATS_URL names other servers than ${file}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`
  );
}, 600_000);

// An Envoy that is on with no NATS server takes no inbound messages either, and the pane's Legion
// claim refuses then (pi-envoy's `requireInbound`), so the rig refuses it the same way.
test("refuses a profile envoy.json whose Envoy has no NATS server", () => {
  const agent = path.join(home, ".omp", "profiles", "rig-no-nats-profile", "agent");
  mkdirSync(agent, { recursive: true });
  writeFileSync(
    path.join(agent, "envoy.json"),
    JSON.stringify({ envoy: { url: "http://rig-listener.internal.example:9020" } })
  );

  expect(() =>
    workerPane(
      { rig, port: 13399, profile: "rig-no-nats-profile" },
      inherited,
      { project: "skillsrig", issue: "LWEVAL-1", role: "tester" },
      { daemonModule: DAEMON_MODULE }
    )
  ).toThrow("envoy: Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)");
}, 600_000);

// The daemon refuses a script as the first gh on its PATH (a gh wrapper would answer in place of
// the claim's App token), and so does the rig's reading of it; a gh the rig names, as an operator
// names one with LEGION_GH_PATH, is the pane's real gh whatever it is, which is how the rig hands
// panes its recording stand-in.
test("refuses a script as the first gh on PATH and takes the gh the rig names", () => {
  const claim = { project: "skillsrig", issue: "LWEVAL-1", role: "tester" } as const;
  const launch = { rig, port: 13399, profile: "rig-profile" };
  const shimmed = { ...inherited, PATH: `${scriptBin}:${outerBin}:/usr/bin` };
  expect(() => workerPane(launch, shimmed, claim, { daemonModule: DAEMON_MODULE })).toThrow(
    `the first gh on the daemon's PATH, ${path.join(scriptBin, "gh")}, is a script`
  );
  const named = workerPane(launch, shimmed, claim, {
    daemonModule: DAEMON_MODULE,
    ghPath: path.join(scriptBin, "gh"),
  }).env;
  expect(named.LEGION_GH_PATH).toBe(path.join(scriptBin, "gh"));
}, 600_000);

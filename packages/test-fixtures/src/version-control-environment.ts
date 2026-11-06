import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

/** The identity jj commits as, which no configuration of the host supplies. */
export const versionControlIdentity = {
  JJ_USER: "Legion Test",
  JJ_EMAIL: "legion-test@example.invalid",
} as const;

/**
 * The names {@link versionControlEnvironment} sets, for a caller that puts them on its own process
 * and hands them to `restoreEnvironment` so the next file finds its own again.
 */
export const versionControlNames = [
  "HOME",
  "XDG_CONFIG_HOME",
  "JJ_CONFIG",
  "JJ_USER",
  "JJ_EMAIL",
  "GIT_CONFIG_GLOBAL",
  "GIT_CONFIG_NOSYSTEM",
  "GIT_TERMINAL_PROMPT",
] as const;

/**
 * The environment a jj or git this suite starts runs in: the given directory as its home, with a
 * `.config` under it and a jj configuration file of its own in it, no git configuration at all,
 * and the identity. So neither the developer's `~/.jjconfig.toml`, nor their `~/.gitconfig`, nor
 * the system gitconfig, nor a prompt for a credential decides what a test sees.
 *
 * It makes the directories and writes the file, and sets nothing on this process: a caller whose
 * code under test starts jj with the process's own environment puts the names on `process.env`
 * itself, and registers their restore.
 *
 * Give it a temporary directory of the suite's own — `temporaryDirectory(prefix)` — so the home is
 * removed with it.
 */
export function versionControlEnvironment(home: string): Record<string, string> {
  const configHome = join(home, ".config");
  mkdirSync(configHome, { recursive: true });
  const config = join(home, "jj-config.toml");
  writeFileSync(config, "# The test's own jj configuration: nothing of the developer's.\n");
  return {
    HOME: home,
    XDG_CONFIG_HOME: configHome,
    JJ_CONFIG: config,
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_TERMINAL_PROMPT: "0",
    ...versionControlIdentity,
  };
}

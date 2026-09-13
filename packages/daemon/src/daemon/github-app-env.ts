import os from "node:os";
import path from "node:path";
import { isSecretLikeName } from "./environment";

const SCRUBBED_ENV_KEYS = ["GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_CONFIG_DIR"];
const SCRUBBED_ENV_PREFIX = "LEGION_GITHUB_APP_";

function isolatedGhConfigDir(baseEnv: NodeJS.ProcessEnv): string {
  const stateHome =
    baseEnv.XDG_STATE_HOME && path.isAbsolute(baseEnv.XDG_STATE_HOME)
      ? baseEnv.XDG_STATE_HOME
      : path.join(os.homedir(), ".local", "state");
  return path.join(stateHome, "legion", "gh");
}

/** Drops every credential-shaped name (`isSecretLikeName`) — the Dispatch bearer and the `*_FILE`
 * pointers a pane carries, the grant file pointer, provider keys, App keys — plus the gh-specific
 * names below: a `gh`/git-identity child has no legitimate use for any of them, whether the base is
 * the daemon's pane environment or the pane `legion gh` runs from. */
function scrubGitHubCredentials(baseEnv: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(baseEnv)) {
    if (
      typeof value === "string" &&
      !SCRUBBED_ENV_KEYS.includes(key) &&
      !key.startsWith(SCRUBBED_ENV_PREFIX) &&
      !isSecretLikeName(key)
    ) {
      env[key] = value;
    }
  }
  return env;
}

export function buildGitHubTokenEnv(token: string, baseEnv: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env = scrubGitHubCredentials(baseEnv);
  env.GH_TOKEN = token;
  env.GH_CONFIG_DIR = isolatedGhConfigDir(baseEnv);
  return env;
}

export function buildRoleEnv(
  token: string,
  gitIdentity: { name: string; email: string },
  baseEnv: NodeJS.ProcessEnv
): NodeJS.ProcessEnv {
  const env = buildGitHubTokenEnv(token, baseEnv);
  env.GIT_AUTHOR_NAME = gitIdentity.name;
  env.GIT_AUTHOR_EMAIL = gitIdentity.email;
  env.GIT_COMMITTER_NAME = gitIdentity.name;
  env.GIT_COMMITTER_EMAIL = gitIdentity.email;
  return env;
}

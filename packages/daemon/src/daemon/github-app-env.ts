import os from "node:os";
import path from "node:path";
import { stripDispatchEnv } from "./environment";

const SCRUBBED_ENV_KEYS = ["GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_CONFIG_DIR"];
const SCRUBBED_ENV_PREFIX = "LEGION_GITHUB_APP_";

function isolatedGhConfigDir(baseEnv: NodeJS.ProcessEnv): string {
  const stateHome =
    baseEnv.XDG_STATE_HOME && path.isAbsolute(baseEnv.XDG_STATE_HOME)
      ? baseEnv.XDG_STATE_HOME
      : path.join(os.homedir(), ".local", "state");
  return path.join(stateHome, "legion", "gh");
}

/** Also strips every pane-secret key (`DISPATCH_TOKEN`/`DISPATCH_URL`, the `*_FILE` pointers, the
 * boot token and controller secret, and the retired `DISPATCH_MCP_URL` alias — see
 * `stripDispatchEnv`'s doc comment): a `gh`/git-identity child spawned for a scoped GitHub App
 * role has no legitimate use for a Dispatch bearer or a pane's boot secret, whether or not the
 * daemon's own process — or the pane this call happens to run from — carries one. */
function scrubGitHubCredentials(baseEnv: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const withoutDispatch = stripDispatchEnv(baseEnv);
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(withoutDispatch)) {
    if (
      typeof value === "string" &&
      !SCRUBBED_ENV_KEYS.includes(key) &&
      !key.startsWith(SCRUBBED_ENV_PREFIX)
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

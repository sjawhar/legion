const SCRUBBED_ENV_KEYS = ["GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_CONFIG_DIR"];
const SCRUBBED_ENV_PREFIX = "LEGION_GITHUB_APP_";

function scrubGitHubCredentials(baseEnv: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(baseEnv)) {
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
  env.GH_CONFIG_DIR = "/dev/null";
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

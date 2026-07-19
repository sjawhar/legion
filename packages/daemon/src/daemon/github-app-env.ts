const SCRUBBED_ENV_KEYS = ["GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_CONFIG_DIR"];
const SCRUBBED_ENV_PREFIX = "LEGION_GITHUB_APP_";

export function buildRoleEnv(
  token: string,
  gitIdentity: { name: string; email: string },
  baseEnv: Record<string, string>
): Record<string, string> {
  const env: Record<string, string> = {};
  for (const [key, value] of Object.entries(baseEnv)) {
    if (!SCRUBBED_ENV_KEYS.includes(key) && !key.startsWith(SCRUBBED_ENV_PREFIX)) {
      env[key] = value;
    }
  }
  env.GH_TOKEN = token;
  env.GH_CONFIG_DIR = "/dev/null";
  env.GIT_AUTHOR_NAME = gitIdentity.name;
  env.GIT_AUTHOR_EMAIL = gitIdentity.email;
  env.GIT_COMMITTER_NAME = gitIdentity.name;
  env.GIT_COMMITTER_EMAIL = gitIdentity.email;
  return env;
}

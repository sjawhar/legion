export interface ExecConfig {
  command: string;
  args?: string[];
  env?: Array<{ name: string; value: string }>;
  apiVersion: string;
}

export interface ExecCredentialDeps {
  run(
    argv: string[],
    env: Record<string, string>
  ): Promise<{
    stdout: string;
    exitCode: number;
    stderr: string;
  }>;
  now(): number;
}

export interface TokenProvider {
  token(): Promise<string>;
  invalidate(): void;
}

const EXPIRY_MARGIN_MS = 60_000;

function requireString(value: unknown, field: string, kubeconfigPath: string): string {
  if (typeof value !== "string" || value === "") {
    throw new Error(`${kubeconfigPath}: users[].user.exec.${field} is required`);
  }
  return value;
}

/** Reads the kubeconfig's optional client-go exec-plugin configuration. */
export function parseExecConfig(
  user: Record<string, unknown>,
  kubeconfigPath: string
): ExecConfig | undefined {
  const value = user.exec;
  if (value === undefined) return undefined;
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${kubeconfigPath}: users[].user.exec must be a mapping`);
  }
  const exec = value as Record<string, unknown>;
  const command = requireString(exec.command, "command", kubeconfigPath);
  const apiVersion = requireString(exec.apiVersion, "apiVersion", kubeconfigPath);
  const args = exec.args;
  if (args !== undefined && (!Array.isArray(args) || args.some((arg) => typeof arg !== "string"))) {
    throw new Error(`${kubeconfigPath}: users[].user.exec.args must be an array of strings`);
  }
  const env = exec.env;
  if (
    env !== undefined &&
    (!Array.isArray(env) ||
      env.some(
        (entry) =>
          typeof entry !== "object" ||
          entry === null ||
          typeof (entry as Record<string, unknown>).name !== "string" ||
          typeof (entry as Record<string, unknown>).value !== "string"
      ))
  ) {
    throw new Error(
      `${kubeconfigPath}: users[].user.exec.env must be an array of name/value entries`
    );
  }
  return {
    command,
    apiVersion,
    ...(args !== undefined ? { args } : {}),
    ...(env !== undefined ? { env: env as Array<{ name: string; value: string }> } : {}),
  };
}

/** Executes and caches Kubernetes client-go ExecCredential tokens. */
export function execCredentialProvider(exec: ExecConfig, deps: ExecCredentialDeps): TokenProvider {
  let cached: { token: string; expiresAt: number } | undefined;
  const argv = [exec.command, ...(exec.args ?? [])];
  const env = Object.fromEntries((exec.env ?? []).map((entry) => [entry.name, entry.value]));
  const command = argv.join(" ");

  return {
    async token() {
      if (cached && deps.now() < cached.expiresAt - EXPIRY_MARGIN_MS) return cached.token;

      const result = await deps.run(argv, env);
      if (result.exitCode !== 0) {
        throw new Error(
          `kubeconfig exec plugin \`${command}\` exited ${result.exitCode}: ${result.stderr.trim()}`
        );
      }
      const parsed = JSON.parse(result.stdout) as {
        status?: { token?: string; expirationTimestamp?: string };
      };
      const token = parsed.status?.token;
      if (typeof token !== "string" || token === "") {
        throw new Error(`kubeconfig exec plugin \`${command}\` returned no status.token`);
      }
      const expirationTimestamp = parsed.status?.expirationTimestamp;
      const expiresAt = expirationTimestamp
        ? Date.parse(expirationTimestamp)
        : deps.now() + 600_000;
      cached = { token, expiresAt };
      console.info(
        `[legion] kubeconfig exec plugin minted a token (expires ${new Date(expiresAt).toISOString()})`
      );
      return token;
    },
    invalidate() {
      cached = undefined;
    },
  };
}

interface GitHubAppCredentialsHookOptions {
  fetchFn?: typeof fetch;
  now?: () => number;
}

interface CachedCredentials {
  workerId: string;
  env: Record<string, string>;
  expiresAt: number;
}

interface WorkerRecord {
  id: string;
  sessionId: string;
  workspace: string;
}

interface TokenResponse {
  role: string;
  owner: string;
  expiresAt: string;
  env: Record<string, string>;
}

const credentialCache = new Map<string, CachedCredentials>();
const REFRESH_WINDOW_MS = 5 * 60 * 1000;

export function createGitHubAppCredentialsHook(opts?: GitHubAppCredentialsHookOptions) {
  const fetchFn = opts?.fetchFn ?? globalThis.fetch;
  const nowFn = opts?.now ?? Date.now;

  return async function githubAppCredentialsHook(
    input: { cwd: string; sessionID?: string },
    output: { env: Record<string, string> }
  ): Promise<void> {
    const daemonPort = output.env.LEGION_DAEMON_PORT ?? process.env.LEGION_DAEMON_PORT;
    if (!daemonPort) {
      return;
    }

    let workerId: string | undefined;

    try {
      const workersRes = await fetchFn(`http://127.0.0.1:${daemonPort}/workers`);
      if (!workersRes.ok) {
        return;
      }

      const workers = (await workersRes.json()) as WorkerRecord[];

      if (input.sessionID) {
        const match = workers.find((worker) => worker.sessionId === input.sessionID);
        if (match) {
          workerId = match.id;
        }
      }

      if (!workerId && input.cwd) {
        const match = workers.find((worker) => input.cwd.startsWith(worker.workspace));
        if (match) {
          workerId = match.id;
        }
      }
    } catch {
      return;
    }

    if (!workerId) {
      return;
    }

    const cached = credentialCache.get(workerId);
    if (cached && cached.expiresAt - nowFn() > REFRESH_WINDOW_MS) {
      Object.assign(output.env, cached.env);
      return;
    }

    try {
      const tokenRes = await fetchFn(
        `http://127.0.0.1:${daemonPort}/workers/${encodeURIComponent(workerId)}/token`
      );
      if (!tokenRes.ok) {
        return;
      }

      const data = (await tokenRes.json()) as TokenResponse;
      credentialCache.set(workerId, {
        workerId,
        env: data.env,
        expiresAt: new Date(data.expiresAt).getTime(),
      });

      Object.assign(output.env, data.env);
    } catch {
      // Let the shell command continue without credentials.
    }
  };
}

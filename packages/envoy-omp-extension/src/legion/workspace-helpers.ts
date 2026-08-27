import type { RunResult, WorkspaceCommandOptions } from "@legion/workspace";

export async function setJjIdentity(cwd: string, gitName: string, gitEmail: string): Promise<void> {
  for (const [key, value] of [
    ["user.name", gitName],
    ["user.email", gitEmail],
  ] as const) {
    const child = Bun.spawn(["jj", "config", "set", "--repo", key, JSON.stringify(value)], {
      cwd,
      stdout: "ignore",
      stderr: "pipe",
    });
    const exitCode = await child.exited;
    if (exitCode === 0) continue;
    const stderr = await new Response(child.stderr).text();
    throw new Error(`jj config set ${key} failed: ${stderr.trim()}`);
  }
}
export async function deleteEnvoyInterest(baseUrl: string, sessionID: string): Promise<void> {
  const response = await fetch(`${baseUrl}/v1/interests/${encodeURIComponent(sessionID)}`, {
    method: "DELETE",
  });
  const responseBody = await response.text();
  if (!response.ok) {
    throw new Error(
      `DELETE /v1/interests/${sessionID} failed with ${response.status}: ${responseBody}`
    );
  }
}

export async function runWorkspaceCommand(
  cmd: string[],
  opts?: WorkspaceCommandOptions
): Promise<RunResult> {
  const child = Bun.spawn(cmd, {
    ...(opts?.cwd === undefined ? {} : { cwd: opts.cwd }),
    ...(opts?.env === undefined ? {} : { env: { ...process.env, ...opts.env } }),
    stdout: "pipe",
    stderr: "pipe",
  });
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { exitCode, stdout, stderr };
}

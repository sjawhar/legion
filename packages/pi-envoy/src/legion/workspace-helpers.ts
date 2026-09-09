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

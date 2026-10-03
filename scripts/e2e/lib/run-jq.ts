/** Runs `jq` with `args` over `stdin` and returns what it printed; a jq failure throws its stderr. */
export function runJq(args: readonly string[], stdin: string): string {
  const run = Bun.spawnSync(["jq", ...args], { stdin: new TextEncoder().encode(stdin) });
  if (run.exitCode !== 0) throw new Error(`jq exited ${run.exitCode}: ${run.stderr.toString()}`);
  return run.stdout.toString();
}

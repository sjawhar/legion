// Shared by the generators that read a table a Go binary prints about itself: envoy-dispatch's
// routes and settings, envoy-listener's routes and settings. scripts/generate.ts puts the binaries
// scripts/build-binaries.sh built from this commit first on PATH. To run such a generator alone,
// build the binaries into a directory and put that first on PATH:
// `docs/site/scripts/build-binaries.sh <dir> && PATH=<dir>:$PATH bun <generator> <out>`.

/** Runs `<binary> <subcommand>` from PATH and returns the rows its JSON prints under `key`. It
 *  refuses a missing binary, a non-zero exit, no rows, and a row without a non-empty string in each
 *  of `fields`; each generator checks the rest of its rows' shape itself. */
export function readBinaryTable(
  binary: string,
  subcommand: string,
  key: string,
  fields: readonly string[]
): Record<string, unknown>[] {
  const path = Bun.which(binary);
  if (path === null) {
    throw new Error(
      `${binary} is not on PATH: run this generator through docs/site/scripts/generate.ts, which builds it (scripts/build-binaries.sh) and puts it there`
    );
  }
  const command = `${binary} ${subcommand}`;
  const run = Bun.spawnSync([path, subcommand], { stdout: "pipe", stderr: "pipe" });
  if (run.exitCode !== 0) {
    throw new Error(`${command} exited ${run.exitCode}:\n${run.stderr.toString()}`);
  }
  const rows = (JSON.parse(run.stdout.toString()) as Record<string, unknown>)[key];
  if (!Array.isArray(rows) || rows.length === 0) {
    throw new Error(`${command} printed no ${key}`);
  }
  for (const row of rows) {
    for (const field of fields) {
      if (typeof row?.[field] !== "string" || row[field] === "") {
        throw new Error(`${command}: ${JSON.stringify(row)} has no ${field}`);
      }
    }
  }
  return rows;
}

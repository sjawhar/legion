/** The OMP fork build every Legion process runs — the one mise tool spec `DEFAULT_OMP_INVOCATION`
 * wraps and the worker image resolves (packages/daemon/docker/worker.Dockerfile runs this file with
 * `bun` and reads its one-line output). Bump the pin here and nowhere else. */
export const OMP_FORK_PIN = "github:sjawhar/oh-my-pi@18.1.15-sami.20260908-220934";
export const DEFAULT_OMP_INVOCATION = `mise x ${OMP_FORK_PIN} -- omp`;

if (import.meta.main) process.stdout.write(`${OMP_FORK_PIN}\n`);

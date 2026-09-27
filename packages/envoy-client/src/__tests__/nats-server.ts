import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";

/** The NATS server image the tests run; CI pulls it before the tests. */
const NATS_IMAGE = "nats:2.10";

export interface NatsServer {
  readonly url: string;
  stop(): void;
}

/** Runs `docker args…` and returns its stdout and stderr, trimmed, throwing on a non-zero exit. */
async function docker(...args: string[]): Promise<{ stdout: string; stderr: string }> {
  const run = Bun.spawn(["docker", ...args], { stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(run.stdout).text(),
    new Response(run.stderr).text(),
    run.exited,
  ]);
  if (exitCode !== 0) throw new Error(`docker ${args.join(" ")} exited ${exitCode}: ${stderr}`);
  return { stdout: stdout.trim(), stderr: stderr.trim() };
}

/** Runs a NATS server container of its own, uniquely named, with JetStream; `nkeyUser`, when
 * given, is the public key of the only nkey user it admits. `stop` removes the container. */
export async function startNatsServer(nkeyUser?: string): Promise<NatsServer> {
  const dir = mkdtempSync(path.join(os.tmpdir(), "nats-server-"));
  const config = path.join(dir, "nats.conf");
  writeFileSync(
    config,
    nkeyUser === undefined
      ? "jetstream {}\n"
      : `jetstream {}\nauthorization {\n  users = [ { nkey: ${JSON.stringify(nkeyUser)} } ]\n}\n`,
    { mode: 0o644 }
  );
  const name = `legion-nats-test-${crypto.randomUUID()}`;
  const stop = () => {
    Bun.spawnSync(["docker", "rm", "-f", name], { stdout: "ignore", stderr: "ignore" });
    rmSync(dir, { recursive: true, force: true });
  };
  try {
    await docker(
      "run",
      "-d",
      "--name",
      name,
      "-p",
      "127.0.0.1::4222",
      "-v",
      `${config}:/etc/nats/test.conf:ro`,
      NATS_IMAGE,
      "-c",
      "/etc/nats/test.conf"
    );
    const deadline = Date.now() + 90_000;
    // nats-server logs to stderr.
    while (!(await docker("logs", name)).stderr.includes("Server is ready")) {
      if (Date.now() > deadline) throw new Error(`NATS container ${name} never became ready`);
      await Bun.sleep(100);
    }
    const port = (await docker("port", name, "4222/tcp")).stdout.split("\n")[0];
    return { url: `nats://${port}`, stop };
  } catch (error) {
    stop();
    throw error;
  }
}

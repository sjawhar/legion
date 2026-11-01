/** Each lane's Postgres server, from one of the two providers a caller names.
 *
 *  A lane is a whole harness of its own (`e2e/run.ts`), and `e2e/seed.ts` truncates its database
 *  before every row, so two lanes cannot share one: the run starts a server per lane and hands it
 *  to that lane's Dispatch servers and psql as `DATABASE_URL`.
 *
 *  - `DISPATCH_E2E_PG_IMAGE` is a `docker run` of the image the caller names, which is CI's path:
 *    the job already pulls the pgvector image its `postgres` service runs, and a runner has
 *    Docker but no Postgres binaries.
 *  - `DISPATCH_E2E_PG_BIN` is a directory holding `initdb` and `pg_ctl`, which is a developer's
 *    and an issue pod's path, where there is a Postgres installation and no Docker.
 *
 *  Both keep the lane's data under the run's own temporary directory, which `e2e/run.ts` removes
 *  when it ends, and both answer on 127.0.0.1: the server's dev sign-in refuses a `DATABASE_URL`
 *  whose host is neither loopback nor a unix socket (`e2e/run-server.sh`).
 *
 *  The binaries provider runs `initdb` once per run and copies the data directory it made per
 *  lane, because `initdb` is the cost that grows with the lanes: measured in an issue pod, from
 *  nothing to every lane answering `select 1`, `initdb` per lane took 3.7-4.1 s at 2 lanes and
 *  7.1-8.3 s at 4, against 4.5-6.0 s and 5.9-6.2 s for one `initdb` and a copy per lane, which
 *  is flat in the lanes because only the copies grow. The template carries the lane's database
 *  and its pgvector extension, which migration 0072 needs, and not the migrations: the server
 *  applies those at boot, as it does today against a fresh database, and a template the same pod
 *  measured with them applied cost 5.7-6.2 s at 2 lanes and 7.7-8.7 s at 4 — more than it saves,
 *  since the 1.0 s of migrations it takes out of a lane's boot runs in every lane at once. */
import { execFile } from "node:child_process";
import { existsSync, mkdirSync } from "node:fs";
import { createServer } from "node:net";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { promisify } from "node:util";

import { sqlAt } from "./psql";

const execFileAsync = promisify(execFile);

/** A `docker run` of this image serves one lane. */
export const pgImageVariable = "DISPATCH_E2E_PG_IMAGE";
/** This directory's `initdb` and `pg_ctl` serve every lane. */
export const pgBinVariable = "DISPATCH_E2E_PG_BIN";

/** The database every lane's server holds, named the same in each: the lanes are separate
 *  clusters, and nothing outside a lane reads it. */
const databaseName = "dispatch_e2e";
/** A lane's connection limit. One Dispatch process opens up to `store.ConnectionsPerProcess`
 *  (89, packages/envoy/internal/dispatch/store/pool.go), two-process mode runs two of them on
 *  one lane, the harness's psql sessions open their own, and Postgres reserves 3 for superusers:
 *  200 clears all of that with room for a pool that has not closed yet. */
const maxConnections = 200;
/** The lane's data dies with the run, so its server trades crash safety for speed. */
const settings = [
  `max_connections=${maxConnections}`,
  "fsync=off",
  "full_page_writes=off",
  "synchronous_commit=off",
];
/** How long a lane's server may take to answer `select 1`. */
const readyMs = 120_000;
/** How many ports a cluster tries before it gives up (see `freePort`). */
const bindAttempts = 3;

/** One lane's Postgres. */
export interface LaneDatabase {
  /** The `DATABASE_URL` this lane's servers and psql use. */
  readonly url: string;
  /** The port it serves on, which the harness logs and no other lane shares. */
  readonly port: number;
  stop(): Promise<void>;
}

/** The provider a run starts every lane's Postgres from. */
export interface LanePostgres {
  /** What a log line and a refusal call it. */
  readonly name: string;
  /** One lane's server. Everything it leaves behind is under the run's own directory, which
   *  `e2e/run.ts` removes, or a container the returned `stop` removes. */
  start(lane: number): Promise<LaneDatabase>;
}

/** A port the OS chose and nothing holds. Another listener can take it between here and the
 *  moment the server binds it, which is why `startCluster` and the image provider try another
 *  port rather than failing: a Postgres server cannot inherit the socket the way the harness's
 *  own listeners do (`scripts/e2e/lib/held-port.py`). */
async function freePort(): Promise<number> {
  const socket = createServer();
  const chosen = Promise.withResolvers<number>();
  socket.once("error", chosen.reject);
  socket.listen(0, "127.0.0.1", () => {
    const address = socket.address();
    if (address === null || typeof address === "string") {
      chosen.reject(new Error("the OS named no port for a listening socket"));
    } else {
      chosen.resolve(address.port);
    }
  });
  try {
    return await chosen.promise;
  } finally {
    const closed = Promise.withResolvers<void>();
    socket.close(() => closed.resolve());
    await closed.promise;
  }
}

/** Waits until `url` answers `select 1`: a server that is up has read its data directory, run
 *  its own recovery and opened its port. */
async function ready(url: string, what: string): Promise<void> {
  const deadline = Date.now() + readyMs;
  for (;;) {
    try {
      await sqlAt(url, ["select 1"]);
      return;
    } catch (error) {
      if (Date.now() > deadline) {
        throw new Error(
          `The Dispatch e2e harness cannot start: ${what} did not answer within ` +
            `${readyMs / 1000} s: ${error instanceof Error ? error.message : String(error)}`
        );
      }
      await sleep(200);
    }
  }
}

/** Starts the cluster in `data`, with its log and its unix socket in `dir`, and returns the port
 *  it serves on. */
async function startCluster(bin: string, data: string, dir: string, what: string): Promise<number> {
  let last = "";
  for (let attempt = 1; attempt <= bindAttempts; attempt++) {
    const port = await freePort();
    const options = [`-p ${port}`, "-h 127.0.0.1", `-k ${dir}`, ...settings.map((s) => `-c ${s}`)];
    try {
      await execFileAsync(join(bin, "pg_ctl"), [
        "-D",
        data,
        "-l",
        join(dir, "postgres.log"),
        "-o",
        options.join(" "),
        "-w",
        "start",
      ]);
      return port;
    } catch (error) {
      last = error instanceof Error ? error.message : String(error);
    }
  }
  throw new Error(
    `The Dispatch e2e harness cannot start: ${what} did not start on any of ${bindAttempts} ` +
      `ports (${join(dir, "postgres.log")}): ${last}`
  );
}

function binariesProvider(dir: string, bin: string): LanePostgres {
  for (const tool of ["initdb", "pg_ctl"]) {
    if (!existsSync(join(bin, tool))) {
      throw new Error(
        `The Dispatch e2e harness cannot start: ${pgBinVariable}=${bin} holds no ${tool}. It ` +
          "must name the directory of a Postgres installation, the one holding initdb and pg_ctl."
      );
    }
  }
  /** The one `initdb` of the run, with the lane database and its pgvector extension, copied per
   *  lane. Started once however many lanes ask for it. */
  let template: Promise<string> | undefined;
  const makeTemplate = async (): Promise<string> => {
    const templateDir = join(dir, "template");
    const data = join(templateDir, "pgdata");
    mkdirSync(templateDir, { recursive: true });
    await execFileAsync(join(bin, "initdb"), [
      "-D",
      data,
      "-U",
      "postgres",
      "--auth=trust",
      "--no-sync",
      "-E",
      "UTF8",
    ]);
    const port = await startCluster(bin, data, templateDir, "the lanes' Postgres template");
    const admin = `postgres://postgres@127.0.0.1:${port}/postgres?sslmode=disable`;
    await ready(admin, "the lanes' Postgres template");
    await sqlAt(admin, [`create database ${databaseName}`]);
    await sqlAt(`postgres://postgres@127.0.0.1:${port}/${databaseName}?sslmode=disable`, [
      "create extension vector",
    ]);
    await execFileAsync(join(bin, "pg_ctl"), ["-D", data, "-m", "fast", "-w", "stop"]);
    return data;
  };
  return {
    name: `the Postgres binaries in ${bin}`,
    async start(lane: number): Promise<LaneDatabase> {
      template ??= makeTemplate();
      const laneDir = join(dir, `lane-${lane}`);
      const data = join(laneDir, "pgdata");
      mkdirSync(laneDir, { recursive: true });
      // `cp -a` keeps the 0700 a data directory's server refuses to start without.
      await execFileAsync("cp", ["-a", await template, data]);
      const what = `lane ${lane}'s Postgres`;
      const port = await startCluster(bin, data, laneDir, what);
      const url = `postgres://postgres@127.0.0.1:${port}/${databaseName}?sslmode=disable`;
      await ready(url, what);
      return {
        port,
        url,
        stop: async () => {
          await execFileAsync(join(bin, "pg_ctl"), ["-D", data, "-m", "immediate", "-w", "stop"]);
        },
      };
    },
  };
}

/** The container's own last lines, for a refusal that would otherwise say only that nothing
 *  answered: a server that exits at once leaves its reason there and nowhere else. */
async function containerLog(container: string): Promise<string> {
  try {
    const { stdout, stderr } = await execFileAsync("docker", ["logs", "--tail", "20", container]);
    const lines = `${stdout}${stderr}`.trim();
    return lines === "" ? "it logged nothing" : lines;
  } catch (error) {
    return `its log could not be read: ${error instanceof Error ? error.message : String(error)}`;
  }
}

function imageProvider(image: string, run: string): LanePostgres {
  return {
    name: `the Postgres image ${image}`,
    async start(lane: number): Promise<LaneDatabase> {
      const what = `lane ${lane}'s Postgres`;
      let last = "";
      for (let attempt = 1; attempt <= bindAttempts; attempt++) {
        // The name carries the attempt: a container whose port was taken keeps its name until it
        // is removed, and the run removes it with the lane.
        const container = `dispatch-e2e-pg-${run}-${lane}-${attempt}`;
        const port = await freePort();
        try {
          await execFileAsync("docker", [
            "run",
            "--detach",
            "--name",
            container,
            "--publish",
            `127.0.0.1:${port}:5432`,
            "--env",
            "POSTGRES_PASSWORD=dispatch",
            "--env",
            `POSTGRES_DB=${databaseName}`,
            "--env",
            "POSTGRES_INITDB_ARGS=--no-sync",
            // The data stays in the container's own writable layer and goes with it. A bind
            // mount of a directory this process made is what the image cannot use: its server
            // runs as the image's own user, which may neither write nor chown a directory the
            // runner's user owns, and the container then exits before it listens.
            "--tmpfs",
            "/var/lib/postgresql/data:rw,exec",
            image,
            ...settings.flatMap((setting) => ["-c", setting]),
          ]);
          const url = `postgres://postgres:dispatch@127.0.0.1:${port}/${databaseName}?sslmode=disable`;
          await ready(url, what);
          // The image carries the extension; the database the image made still has to install it,
          // as migration 0072 needs it.
          await sqlAt(url, ["create extension if not exists vector"]);
          return {
            port,
            url,
            stop: async () => {
              await execFileAsync("docker", ["rm", "--force", "--volumes", container]);
            },
          };
        } catch (error) {
          last = `${error instanceof Error ? error.message : String(error)}; the container said: ${await containerLog(container)}`;
          await execFileAsync("docker", ["rm", "--force", "--volumes", container]).catch(() => {});
        }
      }
      throw new Error(
        `The Dispatch e2e harness cannot start: ${what} did not come up on any of ` +
          `${bindAttempts} ports from ${image}: ${last}`
      );
    },
  };
}

/** The provider this run starts its lanes' Postgres from, or undefined when the caller named
 *  none, which is a run against the `DATABASE_URL` it was given. */
export function lanePostgres(options: {
  /** The run's own temporary directory, which it removes when it ends. */
  readonly dir: string;
  /** What names this run's containers apart from another run's on one machine. */
  readonly run: string;
  readonly env?: NodeJS.ProcessEnv;
}): LanePostgres | undefined {
  const env = options.env ?? process.env;
  const image = (env[pgImageVariable] ?? "").trim();
  const bin = (env[pgBinVariable] ?? "").trim();
  if (image !== "" && bin !== "") {
    throw new Error(
      `${pgImageVariable} and ${pgBinVariable} both name a Postgres for this run's lanes. Set ` +
        "one of them: the image is a `docker run` per lane, the directory a server per lane from " +
        "its initdb and pg_ctl."
    );
  }
  if (image !== "") return imageProvider(image, options.run);
  if (bin !== "") return binariesProvider(options.dir, bin);
  return undefined;
}

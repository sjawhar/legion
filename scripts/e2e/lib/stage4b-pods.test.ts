import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runJq } from "./run-jq";
import { scriptFunctions } from "./script-functions";

// Stage 4b's reads of a pod's addresses: the pod shape's --connect rule (shape_problems and
// record_stream), taken from the script by name (script-functions.ts) and run on the runtime's
// golden root issue pod, whose six role launchers dial tcp://192.0.2.250:13371
// (packages/daemon/internal/runtime/sandbox/testdata/golden).
const root = join(import.meta.dir, "..", "..", "..");
const lib = join(root, "scripts", "e2e", "lib");
const fn = scriptFunctions(join(root, "scripts", "e2e", "stage4b-sandbox-tree.sh"));
const shapeProblems = fn("shape_problems");
const dir = mkdtempSync(join(tmpdir(), "stage4b-pods-test."));
afterAll(() => rmSync(dir, { recursive: true, force: true }));
let runs = 0;
const golden = JSON.parse(
  readFileSync(
    join(
      root,
      "packages",
      "daemon",
      "internal",
      "runtime",
      "sandbox",
      "testdata",
      "golden",
      "root.json"
    ),
    "utf8"
  )
);
// The issue pod's role containers, in the order the runtime builds them (claim.Roles).
const roles = ["architect", "planner", "implementer", "tester", "reviewer", "merger"];
const goldenStream = "tcp://192.0.2.250:13371";
// A moved stream: a restart on the run's other port, so the worker stream takes the API's.
const movedStream = "tcp://192.0.2.250:13370";
const created = "2026-10-06T12:00:00Z";
const moved = "2026-10-06T12:30:00Z";

interface Container {
  name: string;
  command?: string[];
}
// withConnect is an argv edit that points a role launcher's --connect at STREAM.
const withConnect = (stream: string) => (command: string[]) =>
  command.map((word, i) => (command[i - 1] === "--connect" ? stream : word));
// pod is the golden root issue pod created at AT, the command of each role container ONLY names
// (every role's by default) edited by argv.
function pod(
  argv: (command: string[]) => string[] = (command) => command,
  at = created,
  only: string[] = roles
) {
  const spec = structuredClone(golden.spec.podTemplate.spec);
  spec.containers = spec.containers.map((container: Container) =>
    only.includes(container.name)
      ? { ...container, command: argv(container.command ?? []) }
      : container
  );
  return { metadata: { name: "pod", uid: "uid", creationTimestamp: at }, spec };
}
// eachLauncher is the line shape_problems prints for every role launcher, in container order.
const eachLauncher = (line: (role: string) => string) => roles.map(line);

interface Stream {
  since: string;
  stream: string;
}
interface Run {
  // host is advertise_host; port is the worker stream port the calling shell holds now (the shape
  // watcher forked before a restart that moved the stream holds the old one, the main shell after
  // it the new one).
  host?: string;
  port?: number;
  // streams is what record_stream wrote as each daemon started; by default one daemon serving
  // advertise_host at the port since before the pod was created.
  streams?: Stream[];
}
// connectProblems runs shape_problems on the pod and prints only its --connect lines: the golden
// pod departs from the run's other rules (its route ConfigMap and audience are the golden's own),
// which are not under test here.
function connectProblems(object: object, run: Run = {}): string[] {
  const host = run.host ?? "192.0.2.250";
  const port = run.port ?? 13371;
  const evidence = join(dir, `run-${++runs}`);
  const streams = run.streams ?? [
    { since: "2026-10-06T11:00:00Z", stream: `tcp://${host}:${port}` },
  ];
  mkdirSync(evidence);
  writeFileSync(
    join(evidence, "worker-streams.jsonl"),
    streams.map((s) => `${JSON.stringify(s)}\n`).join("")
  );
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
root=${JSON.stringify(root)} evidence=${JSON.stringify(evidence)} route_configmap=route gateway_audience=audience host=${host} port_worker_stream=${port}
${shapeProblems}
shape_problems`,
    ],
    { stdin: new TextEncoder().encode(JSON.stringify(object)) }
  );
  if (result.exitCode !== 0)
    throw new Error(`shape_problems exited ${result.exitCode}: ${result.stderr}`);
  return result.stdout
    .toString()
    .split("\n")
    .filter((line) => line.includes("launcher dials") || line.includes("launchers dial"));
}

describe("the pod shape's --connect rule", () => {
  test("a pod whose role launchers dial advertise_host at the worker stream port passes it", () => {
    expect(connectProblems(pod())).toEqual([]);
  });

  test("a pod told the address the daemon binds departs, naming each launcher and that address", () => {
    const wildcard = pod(withConnect("tcp://0.0.0.0:13371"));
    expect(connectProblems(wildcard)).toEqual(
      eachLauncher(
        (role) =>
          `the ${role} launcher dials tcp://0.0.0.0:13371, not advertise_host at ${goldenStream}`
      )
    );
  });

  test("one launcher told another address departs alone, naming its role", () => {
    const stray = pod(withConnect("tcp://192.0.2.9:13371"), created, ["reviewer"]);
    expect(connectProblems(stray)).toEqual([
      `the reviewer launcher dials tcp://192.0.2.9:13371, not advertise_host at ${goldenStream}`,
    ]);
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; launcher_connect'], JSON.stringify(stray))
    ).toBe("null\n");
  });

  test("a pod told another advertise_host departs", () => {
    expect(connectProblems(pod(), { host: "192.0.2.9" })).toEqual(
      eachLauncher(
        (role) =>
          `the ${role} launcher dials ${goldenStream}, not advertise_host at tcp://192.0.2.9:13371`
      )
    );
  });

  test("a pod whose launchers have no --connect departs", () => {
    const unconnected = pod((command) =>
      command.filter((word, i) => word !== "--connect" && command[i - 1] !== "--connect")
    );
    expect(connectProblems(unconnected)).toEqual(
      eachLauncher(
        (role) =>
          `the ${role} launcher dials nothing (no --connect), not advertise_host at ${goldenStream}`
      )
    );
    expect(
      runJq(
        ["-r", "-L", lib, 'include "stage4b-pods"; launcher_connect'],
        JSON.stringify(unconnected)
      )
    ).toBe("null\n");
  });

  test("launcher_connect is the one address every role launcher dials", () => {
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; launcher_connect'], JSON.stringify(pod()))
    ).toBe(`${goldenStream}\n`);
  });
});

describe("the pod shape across a move of the worker stream", () => {
  // The run's daemons as record_stream noted them: the first on the golden stream, a later one on
  // the moved stream.
  const streams = [
    { since: "2026-10-06T11:00:00Z", stream: goldenStream },
    { since: moved, stream: movedStream },
  ];
  const later = "2026-10-06T12:31:00Z";

  test("the main shell, on the moved port, holds a pod created before the move to the old stream", () => {
    expect(connectProblems(pod(), { port: 13370, streams })).toEqual([]);
  });

  test("the shape watcher, forked on the old port, holds a pod created after it to the new stream", () => {
    expect(connectProblems(pod(withConnect(movedStream), later), { port: 13371, streams })).toEqual(
      []
    );
  });

  test("a pod created in the move's second is held to the new stream", () => {
    expect(connectProblems(pod(withConnect(movedStream), moved), { streams })).toEqual([]);
  });

  test("a pod created after the move but told the old stream departs, naming the new one", () => {
    expect(connectProblems(pod(undefined, later), { port: 13371, streams })).toEqual(
      eachLauncher(
        (role) => `the ${role} launcher dials ${goldenStream}, not advertise_host at ${movedStream}`
      )
    );
  });

  test("a pod created before the move but told the new stream departs, naming the old one", () => {
    expect(connectProblems(pod(withConnect(movedStream)), { port: 13370, streams })).toEqual(
      eachLauncher(
        (role) => `the ${role} launcher dials ${movedStream}, not advertise_host at ${goldenStream}`
      )
    );
  });

  test("a pod created before any daemon served a stream departs, naming when it was created", () => {
    expect(connectProblems(pod(undefined, "2026-10-06T10:59:59Z"), { streams })).toEqual([
      `the role launchers dial ${goldenStream}, but no worker stream was served when the pod was created (2026-10-06T10:59:59Z)`,
    ]);
  });
});

describe("record_stream", () => {
  // recordStreams runs record_stream once per port, each as a daemon on that worker stream port is
  // about to start, and returns the record and the second each record_stream call began in.
  function recordStreams(ports: number[]) {
    const evidence = join(dir, `run-${++runs}`);
    mkdirSync(evidence);
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `set -Eeuo pipefail
evidence=${JSON.stringify(evidence)} host=192.0.2.250
${fn("record_stream")}
for port_worker_stream in ${ports.join(" ")}; do date -u +%FT%TZ; record_stream; done`,
    ]);
    if (result.exitCode !== 0)
      throw new Error(`record_stream exited ${result.exitCode}: ${result.stderr}`);
    return {
      began: result.stdout.toString().trim().split("\n"),
      record: readFileSync(join(evidence, "worker-streams.jsonl"), "utf8")
        .trim()
        .split("\n")
        .map((line) => JSON.parse(line) as Stream),
    };
  }

  test("notes each daemon's stream, a restart at the same address included", () => {
    const { record } = recordStreams([13371, 13371]);
    expect(record.map((r) => r.stream)).toEqual([goldenStream, goldenStream]);
  });

  test("starts a moved stream in a later second than any pod the stopped daemon created", () => {
    // The second the move began in is the latest a pod the stopped daemon created can carry.
    const { began, record } = recordStreams([13371, 13370]);
    expect(record.map((r) => r.stream)).toEqual([goldenStream, movedStream]);
    expect(record[1].since > began[1]).toBe(true);
  });
});

describe("stage4b-pods.jq's ready_pods", () => {
  // event is one pod watch line: a pod with these labels whose role launchers are ready, all but
  // the one NOT_READY names.
  function event(name: string, labels: object, notReady?: string) {
    return JSON.stringify({
      kind: "Pod",
      object: {
        kind: "Pod",
        metadata: { name, labels },
        status: {
          containerStatuses: roles.map((role) => ({ name: role, ready: role !== notReady })),
        },
      },
    });
  }

  test("keeps a ready issue pod and leaves out the probe, a control, and a pod with a role unready", () => {
    const watch = [
      event("kept", {}),
      event("probe", { "legion.dev/probe": "x" }),
      event("control", { "legion.dev/e2e-control": "x" }),
      event("unready", {}, "merger"),
    ].join("\n");
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; ready_pods | .metadata.name'], watch)
    ).toBe("kept\n");
  });
});

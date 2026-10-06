import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runJq } from "./run-jq";

// The pod shape's --connect rule: Stage 4b's own shape_problems and record_stream, taken from the
// script by name as stage4b-verdict.test.ts takes its functions, run on the runtime's golden root
// pod, whose worker shim dials tcp://192.0.2.250:13371
// (packages/daemon/internal/runtime/sandbox/testdata/golden).
const root = join(import.meta.dir, "..", "..", "..");
const lib = join(root, "scripts", "e2e", "lib");
const script = readFileSync(join(root, "scripts", "e2e", "stage4b-sandbox-tree.sh"), "utf8");
const fn = (name: string) => {
  const found = new RegExp(`^${name}\\(\\) \\{[\\s\\S]*?\\n\\}$`, "m").exec(script);
  if (found === null) throw new Error(`stage4b-sandbox-tree.sh defines no ${name}()`);
  return found[0];
};
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
const goldenStream = "tcp://192.0.2.250:13371";
// The moved stream: address-moved swaps the run's two ports, so the worker stream takes the API's.
const movedStream = "tcp://192.0.2.250:13370";
const created = "2026-10-06T12:00:00Z";
const moved = "2026-10-06T12:30:00Z";

interface Container {
  name: string;
  command?: string[];
}
// withConnect is an argv edit that points the worker shim's --connect at STREAM.
const withConnect = (stream: string) => (command: string[]) =>
  command.map((word, i) => (command[i - 1] === "--connect" ? stream : word));
// pod is the golden root pod created at AT, its worker container's command edited by argv.
function pod(argv: (command: string[]) => string[] = (command) => command, at = created) {
  const spec = structuredClone(golden.spec.podTemplate.spec);
  spec.containers = spec.containers.map((container: Container) =>
    container.name === "worker"
      ? { ...container, command: argv(container.command ?? []) }
      : container
  );
  return { metadata: { name: "pod", uid: "uid", creationTimestamp: at }, spec };
}

interface Stream {
  since: string;
  stream: string;
}
interface Run {
  // host is advertise_host; port is the worker stream port the calling shell holds now (the shape
  // watcher forked before a move holds the old one, the main shell after it the new one).
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
    .filter((line) => line.includes("worker shim"));
}

describe("the pod shape's --connect rule", () => {
  test("a pod whose shim dials advertise_host at the worker stream port passes it", () => {
    expect(connectProblems(pod())).toEqual([]);
  });

  test("a pod told the address the daemon binds departs, naming that address", () => {
    const wildcard = pod(withConnect("tcp://0.0.0.0:13371"));
    expect(connectProblems(wildcard)).toEqual([
      `the worker shim dials tcp://0.0.0.0:13371, not advertise_host at ${goldenStream}`,
    ]);
  });

  test("a pod told another advertise_host departs", () => {
    expect(connectProblems(pod(), { host: "192.0.2.9" })).toEqual([
      `the worker shim dials ${goldenStream}, not advertise_host at tcp://192.0.2.9:13371`,
    ]);
  });

  test("a pod whose shim has no --connect departs", () => {
    const unconnected = pod((command) =>
      command.filter((word, i) => word !== "--connect" && command[i - 1] !== "--connect")
    );
    expect(connectProblems(unconnected)).toEqual([
      `the worker shim dials nothing (no --connect), not advertise_host at ${goldenStream}`,
    ]);
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; shim_connect'], JSON.stringify(unconnected))
    ).toBe("null\n");
  });
});

describe("the pod shape across a move of the worker stream (address-moved)", () => {
  // The run's daemons as record_stream noted them: the first on the golden stream, the one
  // address-moved starts on the moved stream.
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
    expect(connectProblems(pod(undefined, later), { port: 13371, streams })).toEqual([
      `the worker shim dials ${goldenStream}, not advertise_host at ${movedStream}`,
    ]);
  });

  test("a pod created before the move but told the new stream departs, naming the old one", () => {
    expect(connectProblems(pod(withConnect(movedStream)), { port: 13370, streams })).toEqual([
      `the worker shim dials ${movedStream}, not advertise_host at ${goldenStream}`,
    ]);
  });

  test("a pod created before any daemon served a stream departs, naming when it was created", () => {
    expect(connectProblems(pod(undefined, "2026-10-06T10:59:59Z"), { streams })).toEqual([
      `the worker shim dials ${goldenStream}, but no worker stream was served when the pod was created (2026-10-06T10:59:59Z)`,
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
  // event is one pod watch line: a pod with these labels whose worker is ready or not.
  function event(name: string, labels: object, ready: boolean) {
    return JSON.stringify({
      kind: "Pod",
      object: {
        kind: "Pod",
        metadata: { name, labels },
        status: { containerStatuses: [{ name: "worker", ready }] },
      },
    });
  }

  test("keeps a ready Sandbox pod and leaves out the probe, a control, and an unready pod", () => {
    const watch = [
      event("kept", {}, true),
      event("probe", { "legion.dev/probe": "x" }, true),
      event("control", { "legion.dev/e2e-control": "x" }, true),
      event("unready", {}, false),
    ].join("\n");
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; ready_pods | .metadata.name'], watch)
    ).toBe("kept\n");
  });
});

import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runJq } from "./run-jq";
import { scriptFunctions } from "./script-functions";

// Stage 4b's reads of a pod's addresses: the pod shape's --connect rule (shape_problems and
// record_stream) and address-moved's judgement of every claim's pod after the move
// (claims_on_new_addresses), taken from the script by name (script-functions.ts) and run on the
// runtime's golden root pod, whose worker shim dials tcp://192.0.2.250:13371 and which is told
// LEGION_DAEMON_URL http://192.0.2.250:13370
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
const goldenStream = "tcp://192.0.2.250:13371";
// The moved stream: address-moved swaps the run's two ports, so the worker stream takes the API's.
const movedStream = "tcp://192.0.2.250:13370";
const created = "2026-10-06T12:00:00Z";
const moved = "2026-10-06T12:30:00Z";

interface Container {
  name: string;
  command?: string[];
  env?: { name: string; value?: string }[];
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

describe("address-moved: every claim's pod on the new addresses", () => {
  // The run after the move: its two ports swapped, so the worker stream is the API's old port and
  // the API the stream's, and its services as they were (the golden pod's own, and Dispatch).
  const movedDaemonURL = "http://192.0.2.250:13371";
  const dispatch = "https://dispatch.internal.example";
  type Env = NonNullable<Container["env"]>;
  // withEnv is an env edit that sets each of VALUES, adding a variable the env lacks.
  const withEnv = (values: Record<string, string>) => (env: Env) =>
    Object.entries(values).reduce(
      (edited, [name, value]) => [...edited.filter((v) => v.name !== name), { name, value }],
      env
    );
  // movedPod is the golden pod as a claim relaunched after the move runs it, its shim dialling the
  // moved stream and told the moved API and Dispatch, then edited by argv and env.
  function movedPod(argv = withConnect(movedStream), env = (e: Env) => e) {
    const moved = pod(argv);
    moved.spec.containers = moved.spec.containers.map((container: Container) =>
      container.name === "worker"
        ? {
            ...container,
            env: env(
              withEnv({ LEGION_DAEMON_URL: movedDaemonURL, DISPATCH_URL: dispatch })(
                container.env ?? []
              )
            ),
          }
        : container
    );
    return moved;
  }
  // judge runs the script's claims_on_new_addresses over PODS, the operator's reads by pod name,
  // and CLAIMS, what `legion claims` shows (or a read that fails), and returns its exit code, what
  // it printed, and how many times it read `legion claims`.
  function judge(pods: Record<string, object>, claims: { token: string; pod: string }[] | "fails") {
    const run = join(dir, `run-${++runs}`);
    mkdirSync(run);
    writeFileSync(join(run, "pods.json"), JSON.stringify(pods));
    writeFileSync(join(run, "claims.json"), JSON.stringify(claims === "fails" ? [] : claims));
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `set -Eeuo pipefail
root=${JSON.stringify(root)} run=${JSON.stringify(run)} host=192.0.2.250 port_daemon=13371 port_worker_stream=13370
new_stream=${movedStream} new_daemon_url=${movedDaemonURL}
dispatch_base=${dispatch} envoy_url=http://192.0.2.250:9020 nats_url=nats://192.0.2.250:4222
note() { echo "   $*"; }
fail() { echo "FAIL: $*"; exit 1; }
op() { [ "$1 $2 $4 $5" = "get pod -o json" ] && jq -e --arg p "$3" '.[$p]' "$run/pods.json"; }
live_claims() { echo read >>"$run/reads"; ${claims === "fails" ? "return 1" : 'jq -c . "$run/claims.json"'}; }
${fn("pod_connect")}
${fn("pod_env")}
${fn("pod_endpoint_mismatch")}
${fn("on_new_addresses")}
${fn("claims_on_new_addresses")}
claims_on_new_addresses`,
    ]);
    let reads = 0;
    try {
      reads = readFileSync(join(run, "reads"), "utf8").trim().split("\n").length;
    } catch {}
    return { exitCode: result.exitCode, out: result.stdout.toString(), reads };
  }
  const claims = [
    { token: "claim-a", pod: "pod-a" },
    { token: "claim-b", pod: "pod-b" },
  ];

  test("pod_connect is one line, reading the shim's address through shim_connect", () => {
    expect(fn("pod_connect").split("\n")).toHaveLength(1);
    const pods = { connected: pod(), unconnected: pod((c) => c.filter((w) => w !== "--connect")) };
    const result = Bun.spawnSync([
      "bash",
      "-c",
      `set -Eeuo pipefail
root=${JSON.stringify(root)}
op() { jq -e --arg p "$3" '.[$p]' <<<${JSON.stringify(JSON.stringify(pods))}; }
${fn("pod_connect")}
printf '%s|%s\\n' "$(pod_connect connected)" "$(pod_connect unconnected)"`,
    ]);
    expect(result.stdout.toString()).toBe(`${goldenStream}|\n`);
  });

  test("every claim's pod on the new addresses passes, judged from one read of legion claims", () => {
    const { exitCode, out, reads } = judge({ "pod-a": movedPod(), "pod-b": movedPod() }, claims);
    expect(exitCode).toBe(0);
    expect(reads).toBe(1);
    expect(out).toContain(
      `claim-b: pod pod-b dials ${movedStream}, told LEGION_DAEMON_URL ${movedDaemonURL} and the run's services`
    );
    expect(out).toContain("all 2 claims that run a process are on the new addresses");
  });

  test("a claim whose pod keeps the old --connect fails, naming it", () => {
    const { exitCode, out } = judge(
      { "pod-a": movedPod(), "pod-b": movedPod((command) => command) },
      claims
    );
    expect(exitCode).toBe(1);
    expect(out).toEndWith(`FAIL: claim-b's pod pod-b dials ${goldenStream}, not ${movedStream}\n`);
  });

  test("a claim whose pod is told the old API fails, naming it", () => {
    const oldAPI = withEnv({ LEGION_DAEMON_URL: "http://192.0.2.250:13370" });
    const { exitCode, out } = judge(
      { "pod-a": movedPod(), "pod-b": movedPod(undefined, oldAPI) },
      claims
    );
    expect(exitCode).toBe(1);
    expect(out).toEndWith(
      `FAIL: claim-b's pod pod-b has LEGION_DAEMON_URL=http://192.0.2.250:13370, want ${movedDaemonURL}\n`
    );
  });

  test("a claim whose pod is told another Envoy fails, naming the run's input, not the address", () => {
    const otherEnvoy = withEnv({ ENVOY_URL: "http://192.0.2.9:9020" });
    const { exitCode, out } = judge(
      { "pod-a": movedPod(undefined, otherEnvoy), "pod-b": movedPod() },
      claims
    );
    expect(exitCode).toBe(1);
    expect(out).toBe("FAIL: claim-a's pod pod-a has ENVOY_URL differs from LEGION_E2E_ENVOY_URL\n");
  });

  test("a claim whose pod the operator cannot read fails, naming the pod", () => {
    const { exitCode, out } = judge({ "pod-a": movedPod() }, claims);
    expect(exitCode).toBe(1);
    expect(out).toEndWith("FAIL: the operator could not read pod pod-b\n");
  });

  test("a legion claims read that fails fails the check, judging nothing", () => {
    const { exitCode, out, reads } = judge({ "pod-a": movedPod(), "pod-b": movedPod() }, "fails");
    expect(exitCode).toBe(1);
    expect(reads).toBe(1);
    expect(out).toBe("FAIL: legion claims could not be read\n");
  });

  test("a read that shows no claim running a process fails rather than passing empty", () => {
    const { exitCode, out } = judge({}, []);
    expect(exitCode).toBe(1);
    expect(out).toBe("FAIL: legion claims shows no claim that runs a process\n");
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

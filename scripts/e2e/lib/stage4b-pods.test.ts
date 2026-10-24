import { afterAll, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runJq } from "./run-jq";
import { scriptFunctions } from "./script-functions";
import { defaults, type Reservation } from "./stage4b-reservations";

// Stage 4b's reads of a pod's addresses and reservations: the pod shape's --connect rule and its
// reservation rule (shape_problems, with record_stream and lib/stage4b-pods.jq), and the
// address-moved checkpoints' readers (claims_on_handed_addresses and the helpers restart-mid-tree's
// negative control and the two moves use), taken from the script by name (script-functions.ts) and
// run on the runtime's golden root issue pod, whose one container, `roles`, runs the launcher of
// every role, dialing tcp://192.0.2.250:13371, and whose every container carries the daemon's
// default issue-pod reservation (packages/daemon/internal/runtime/sandbox/testdata/golden).
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
// The issue pod's workflow roles, in the order the runtime names them (claim.Roles).
const roles = ["architect", "planner", "implementer", "tester", "reviewer", "merger"];
const goldenStream = "tcp://192.0.2.250:13371";
// A moved stream: a restart on the run's other port, so the worker stream takes the API's.
const movedStream = "tcp://192.0.2.250:13370";
const created = "2026-10-06T12:00:00Z";
const moved = "2026-10-06T12:30:00Z";

interface Resources {
  requests?: Record<string, string>;
  limits?: Record<string, string>;
}
interface Container {
  name: string;
  command?: string[];
  resources?: Resources;
}
// withConnect is an argv edit that points the launcher's --connect at STREAM.
const withConnect = (stream: string) => (command: string[]) =>
  command.map((word, i) => (command[i - 1] === "--connect" ? stream : word));
// pod is the golden root issue pod created at AT, Guaranteed as the API server classes it, its
// `roles` container's command edited by ARGV.
function pod(argv: (command: string[]) => string[] = (command) => command, at = created) {
  const spec = structuredClone(golden.spec.podTemplate.spec);
  spec.containers = spec.containers.map((container: Container) =>
    container.name === "roles"
      ? { ...container, command: argv(container.command ?? []) }
      : container
  );
  return {
    metadata: { name: "pod", uid: "uid", creationTimestamp: at },
    spec,
    status: { qosClass: "Guaranteed" },
  };
}
// renamed is a pod() whose one container is named NAME rather than `roles`, as a pod of the layout
// before one container named each launcher's container for its role.
function renamed(name: string) {
  const object = pod();
  object.spec.containers[0].name = name;
  return object;
}
// reserving is a pod() whose container NAME (`roles` or an init container) carries RESOURCES in
// place of its own.
function reserving(name: string, resources: Resources) {
  const object = pod();
  for (const list of [object.spec.containers, object.spec.initContainers]) {
    for (const container of list as Container[]) {
      if (container.name === name) container.resources = resources;
    }
  }
  return object;
}
// reservation is a container's RESOURCES with cpu and memory as both request and limit, and
// ephemeral-storage as STORAGE_REQUEST under the limit STORAGE (the issue pod's defaults when
// omitted).
const reservation = (
  cpu: string,
  memory: string,
  storage = "80Gi",
  storageRequest = "6Gi"
): Resources => ({
  requests: { cpu, memory, "ephemeral-storage": storageRequest },
  limits: { cpu, memory, "ephemeral-storage": storage },
});

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
  // resources is run_resources, each pod's expected reservation; the defaults by default.
  resources?: Record<string, Reservation>;
}
// shapeProblemLines runs shape_problems on the pod under RUN's worker streams and returns every
// line it prints. The golden pod departs from the run's route and audience rules (its route
// ConfigMap and audience are the golden's own), so each rule's tests keep only their own lines.
function shapeProblemLines(object: object, run: Run = {}): string[] {
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
run_resources=${JSON.stringify(JSON.stringify(run.resources ?? defaults))}
${shapeProblems}
shape_problems`,
    ],
    { stdin: new TextEncoder().encode(JSON.stringify(object)) }
  );
  if (result.exitCode !== 0)
    throw new Error(`shape_problems exited ${result.exitCode}: ${result.stderr}`);
  return result.stdout.toString().split("\n");
}
// connectProblems is shape_problems' --connect lines alone.
function connectProblems(object: object, run: Run = {}): string[] {
  return shapeProblemLines(object, run).filter(
    (line) => line.includes("launcher dials") || line.includes("launchers dial")
  );
}
// reservationProblems is shape_problems' reservation lines alone: a container's reservation or
// disk bound, the pod's QoS class, or an affinity.
function reservationProblems(object: object, run: Run = {}): string[] {
  return shapeProblemLines(object, run).filter(
    (line) =>
      line.includes("reserv") ||
      line.includes("ephemeral-storage") ||
      line.includes("qosClass") ||
      line.includes("affinity")
  );
}

describe("the pod shape's --connect rule", () => {
  test("a pod whose launcher dials advertise_host at the worker stream port passes it", () => {
    expect(connectProblems(pod())).toEqual([]);
  });

  test("a pod told the address the daemon binds departs, naming that address", () => {
    expect(connectProblems(pod(withConnect("tcp://0.0.0.0:13371")))).toEqual([
      `the launcher dials tcp://0.0.0.0:13371, not advertise_host at ${goldenStream}`,
    ]);
  });

  test("a pod told another advertise_host departs", () => {
    expect(connectProblems(pod(), { host: "192.0.2.9" })).toEqual([
      `the launcher dials ${goldenStream}, not advertise_host at tcp://192.0.2.9:13371`,
    ]);
  });

  test("a pod whose launcher has no --connect departs", () => {
    const unconnected = pod((command) =>
      command.filter((word, i) => word !== "--connect" && command[i - 1] !== "--connect")
    );
    expect(connectProblems(unconnected)).toEqual([
      `the launcher dials nothing (no --connect), not advertise_host at ${goldenStream}`,
    ]);
    expect(
      runJq(
        ["-r", "-L", lib, 'include "stage4b-pods"; launcher_connect'],
        JSON.stringify(unconnected)
      )
    ).toBe("null\n");
  });

  test("launcher_connect is the address the roles container's launcher dials, and none in a pod without one", () => {
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; launcher_connect'], JSON.stringify(pod()))
    ).toBe(`${goldenStream}\n`);
    expect(
      runJq(
        ["-r", "-L", lib, 'include "stage4b-pods"; launcher_connect'],
        JSON.stringify(renamed("architect"))
      )
    ).toBe("null\n");
  });

  test("a pod of the layout before one container departs, naming its containers", () => {
    expect(shapeProblemLines(renamed("architect"))).toContain(
      'pod containers ["architect"], want only ["roles"]'
    );
  });
});

// ghProblems is shape_problems' gh credential lines alone (LEGION-631): each role's gh-<role>
// volume, its mount and the clone's helper.
function ghProblems(object: object): string[] {
  return shapeProblemLines(object).filter((line) =>
    /gh-|credential-helper|shareProcessNamespace/.test(line)
  );
}

describe("the pod shape's gh credential rule", () => {
  test("the golden issue pod mounts each role's gh files under its own role directory alone", () => {
    expect(ghProblems(pod())).toEqual([]);
  });

  test("a role's gh volume mounted at another role's directory, or twice, departs", () => {
    const crossed = pod();
    const mounts = crossed.spec.containers[0].volumeMounts;
    mounts.find((m: { name: string }) => m.name === "gh-implementer").mountPath =
      "/var/run/legion/roles/reviewer/gh";
    expect(ghProblems(crossed)).toHaveLength(1);
    expect(ghProblems(crossed)[0]).toContain(
      "gh-implementer at /var/run/legion/roles/reviewer/gh read-only"
    );
    expect(ghProblems(crossed)[0]).toEndWith(
      "want each gh-<role> alone, read-only at /var/run/legion/roles/<role>/gh"
    );
    const twice = pod();
    twice.spec.containers[0].volumeMounts.push({
      name: "gh-reviewer",
      mountPath: "/var/run/legion/gh",
      readOnly: true,
    });
    expect(ghProblems(twice)).toHaveLength(1);
  });

  test("an init container mounting a gh volume departs, naming both", () => {
    const leaked = pod();
    leaked.spec.initContainers
      .find((c: Container) => c.name === "workspace-init")
      .volumeMounts.push({ name: "gh-merger", mountPath: "/var/run/legion/gh", readOnly: true });
    expect(ghProblems(leaked)).toEqual(["init container workspace-init mounts gh-merger"]);
  });

  test("a gh volume of another Secret, a missing file, a shared pid namespace and the old helper each depart", () => {
    const departed = pod();
    departed.spec.volumes.find((v: { name: string }) => v.name === "gh-merger").secret.secretName =
      "other";
    departed.spec.volumes.find((v: { name: string }) => v.name === "gh-tester").secret.items.pop();
    departed.spec.shareProcessNamespace = true;
    const init = departed.spec.initContainers.find((c: Container) => c.name === "workspace-init");
    init.command[init.command.indexOf("--credential-helper") + 1] = "!gh auth git-credential";
    expect(ghProblems(departed)).toEqual([
      'gh-tester projects [{"key":"github-hosts-acme","path":"acme/hosts.yml"},{"key":"github-config-acme","path":"acme/config.yml"},{"key":"github-hosts-sjawhar","path":"sjawhar/hosts.yml"}], not <owner>/hosts.yml and <owner>/config.yml for each owner',
      "gh-merger projects Secret other, not the role Secret legion-legion-legion-208-merger-boot its launcher token comes from",
      "shareProcessNamespace is set",
      "the --credential-helper of workspace-init is !gh auth git-credential, not !legion git-credential",
    ]);
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
    expect(connectProblems(pod(undefined, later), { port: 13371, streams })).toEqual([
      `the launcher dials ${goldenStream}, not advertise_host at ${movedStream}`,
    ]);
  });

  test("a pod created before the move but told the new stream departs, naming the old one", () => {
    expect(connectProblems(pod(withConnect(movedStream)), { port: 13370, streams })).toEqual([
      `the launcher dials ${movedStream}, not advertise_host at ${goldenStream}`,
    ]);
  });

  test("a pod created before any daemon served a stream departs, naming when it was created", () => {
    expect(connectProblems(pod(undefined, "2026-10-06T10:59:59Z"), { streams })).toEqual([
      `the launcher dials ${goldenStream}, but no worker stream was served when the pod was created (2026-10-06T10:59:59Z)`,
    ]);
  });
});

describe("the pod shape's reservation rule", () => {
  test("a pod whose every container carries the issue pod's reservation, Guaranteed and with no affinity, passes it", () => {
    expect(reservationProblems(pod())).toEqual([]);
  });

  test("the run's override is what every container is held to, each field on its own", () => {
    // The run overrides the issue pod's cpu and memory alone; the golden pod carries the defaults,
    // so every container departs, its disk bound the default's.
    const overrides = { ...defaults, issue: { ...defaults.issue, cpu: "2950m", memory: "16Gi" } };
    const departs = (name: string) =>
      `container ${name} reserves cpu 3, memory 19Gi, ephemeral-storage 6Gi of 80Gi, not the run's cpu 2950m, memory 16Gi, ephemeral-storage 6Gi of 80Gi for the issue pod`;
    expect(reservationProblems(pod(), { resources: overrides })).toEqual(
      ["workspace-fetch", "workspace-init", "roles"].map(departs)
    );
    const overridden = pod();
    for (const container of [
      ...overridden.spec.initContainers,
      ...overridden.spec.containers,
    ] as Container[]) {
      container.resources = reservation("2.95", "16384Mi", "81920Mi", "6144Mi");
    }
    // Quantities compare as amounts: 2.95 is 2950m, 16384Mi is 16Gi, 81920Mi 80Gi, 6144Mi 6Gi.
    expect(reservationProblems(overridden, { resources: overrides })).toEqual([]);
  });

  test("a container whose request is not its limit departs, naming both", () => {
    expect(
      reservationProblems(
        reserving("roles", {
          requests: { cpu: "3", memory: "8Gi", "ephemeral-storage": "6Gi" },
          limits: { cpu: "3", memory: "19Gi", "ephemeral-storage": "80Gi" },
        })
      )
    ).toEqual([
      'container roles requests {"cpu":"3","memory":"8Gi","ephemeral-storage":"6Gi"} but is limited to {"cpu":"3","memory":"19Gi","ephemeral-storage":"80Gi"}; this run configures request = limit',
    ]);
  });

  test("a container whose ephemeral-storage request exceeds its limit departs, naming both", () => {
    // The disk bound is a request under a limit, not one value as both: a request past the limit
    // is what the API server refuses, and a request below it at the run's values passes.
    expect(
      reservationProblems(reserving("roles", reservation("3", "19Gi", "80Gi", "81Gi")))
    ).toEqual([
      "container roles requests ephemeral-storage 81Gi past its limit 80Gi; the request may not exceed the limit",
    ]);
    expect(reservationProblems(reserving("roles", reservation("3", "19Gi")))).toEqual([]);
    // A request under the limit at other than the run's values is held to the run's.
    expect(
      reservationProblems(reserving("roles", reservation("3", "19Gi", "80Gi", "7Gi")))
    ).toEqual([
      "container roles reserves cpu 3, memory 19Gi, ephemeral-storage 7Gi of 80Gi, not the run's cpu 3, memory 19Gi, ephemeral-storage 6Gi of 80Gi for the issue pod",
    ]);
  });

  test("a container reserving no cpu, memory or ephemeral-storage departs", () => {
    expect(
      reservationProblems(
        reserving("roles", { limits: { cpu: "3", memory: "19Gi", "ephemeral-storage": "80Gi" } })
      )
    ).toEqual([
      'container roles reserves {"limits":{"cpu":"3","memory":"19Gi","ephemeral-storage":"80Gi"}}, want cpu and memory as both request and limit and ephemeral-storage as request and limit',
    ]);
    expect(
      reservationProblems(
        reserving("roles", {
          requests: { cpu: "3", memory: "19Gi" },
          limits: { cpu: "3", memory: "19Gi" },
        })
      )
    ).toEqual([
      'container roles reserves {"requests":{"cpu":"3","memory":"19Gi"},"limits":{"cpu":"3","memory":"19Gi"}}, want cpu and memory as both request and limit and ephemeral-storage as request and limit',
    ]);
    expect(reservationProblems(reserving("workspace-init", {}))).toEqual([
      "container workspace-init reserves {}, want cpu and memory as both request and limit and ephemeral-storage as request and limit",
    ]);
  });

  test("an init container carries the issue pod's one reservation, whichever launch created the pod", () => {
    expect(reservationProblems(reserving("workspace-fetch", reservation("2", "1Gi")))).toEqual([
      "container workspace-fetch reserves cpu 2, memory 1Gi, ephemeral-storage 6Gi of 80Gi, not the run's cpu 3, memory 19Gi, ephemeral-storage 6Gi of 80Gi for the issue pod",
    ]);
  });

  test("a pod that is not Guaranteed, or carries an affinity, departs", () => {
    const burstable = { ...pod(), status: { qosClass: "Burstable" } };
    expect(reservationProblems(burstable)).toEqual(["qosClass Burstable, want Guaranteed"]);
    const pinned = pod();
    pinned.spec.affinity = {
      podAffinity: {
        requiredDuringSchedulingIgnoredDuringExecution: [{ topologyKey: "kubernetes.io/hostname" }],
      },
    };
    expect(reservationProblems(pinned)).toEqual([
      'affinity {"podAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":[{"topologyKey":"kubernetes.io/hostname"}]}}: a Legion pod asks nothing of its placement beyond the pool',
    ]);
  });

  test("quantity reads the API server's forms as amounts", () => {
    expect(
      runJq(
        ["-c", "-L", lib, 'include "stage4b-pods"; map(quantity)'],
        JSON.stringify(["750m", "1", 2, "1.5", "1Gi", "4096Mi", "1k", "2M", "100m", null])
      )
    ).toBe("[0.75,1,2,1.5,1073741824,4294967296,1000,2000000,0.1,null]\n");
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

// runScript runs SCRIPT under bash with errexit and pipefail, as stage4b-sandbox-tree.sh runs.
function runScript(script: string) {
  const result = Bun.spawnSync(["bash", "-c", `set -Eeuo pipefail\n${script}`]);
  return { exitCode: result.exitCode, out: result.stdout.toString() };
}

describe("the address-moved checkpoints' readers", () => {
  // The run after address-moved-stream-new-pod: its two ports swapped, so the worker stream is the
  // API's old port and the API the stream's.
  const movedDaemonURL = "http://192.0.2.250:13371";
  const dispatch = "https://dispatch.internal.example";
  const envoy = "https://envoy.internal.example:8443";
  const nats = "nats://nats.internal.example:4222";
  // handedEnv is what a role generation launched now is told, edited by VALUES.
  const handedEnv = (values: Record<string, string> = {}) => ({
    DISPATCH_URL: dispatch,
    ENVOY_URL: envoy,
    ENVOY_NATS_URL: nats,
    LEGION_DAEMON_URL: movedDaemonURL,
    ...values,
  });
  const movedPod = () => pod(withConnect(movedStream));
  interface Judged {
    pods: Record<string, object>;
    // envs is each role's Oh My Pi environment, keyed by "<pod>/<role>"; a role with none has no
    // Oh My Pi to read.
    envs: Record<string, Record<string, string>>;
    claims: { token: string; pod: string; role: string }[] | "fails";
    envoySource?: string;
    handedEnvoy?: string;
  }
  // judge runs the script's claims_on_handed_addresses over PODS (the operator's reads by pod
  // name), ENVS (pod_env's reads) and CLAIMS (what `legion claims` shows, or a read that fails),
  // and returns its exit code, what it printed, and how many times it read `legion claims`.
  function judge(j: Judged) {
    const run = join(dir, `run-${++runs}`);
    mkdirSync(run);
    writeFileSync(join(run, "pods.json"), JSON.stringify(j.pods));
    writeFileSync(join(run, "envs.json"), JSON.stringify(j.envs));
    writeFileSync(join(run, "claims.json"), JSON.stringify(j.claims === "fails" ? [] : j.claims));
    const result =
      runScript(`root=${JSON.stringify(root)} run=${JSON.stringify(run)} host=192.0.2.250 port_daemon=13371 port_worker_stream=13370
dispatch_base=${dispatch} nats_url=${nats} handed_envoy_url=${j.handedEnvoy ?? envoy} handed_envoy_source=${JSON.stringify(j.envoySource ?? "LEGION_E2E_ENVOY_URL")}
note() { echo "   $*"; }
fail() { echo "FAIL: $*"; exit 1; }
scrub() { cat; }
op() { [ "$1 $2 $4 $5" = "get pod -o json" ] && jq -e --arg p "$3" '.[$p]' "$run/pods.json"; }
pod_env() { jq -er --arg k "$1/$2" '.[$k] | to_entries[] | "\\(.key)=\\(.value)"' "$run/envs.json"; }
live_claims() { echo read >>"$run/reads"; ${j.claims === "fails" ? "return 1" : 'jq -c . "$run/claims.json"'}; }
${fn("pod_connect")}
${fn("pod_endpoint_mismatch")}
${fn("on_handed_addresses")}
${fn("claims_on_handed_addresses")}
claims_on_handed_addresses`);
    let reads = 0;
    try {
      reads = readFileSync(join(run, "reads"), "utf8").trim().split("\n").length;
    } catch {}
    return { exitCode: result.exitCode, out: result.out, reads };
  }
  const claims = [
    { token: "claim-a", pod: "pod-a", role: "architect" },
    { token: "claim-b", pod: "pod-a", role: "tester" },
  ];
  const handed = { "pod-a/architect": handedEnv(), "pod-a/tester": handedEnv() };

  test("pod_connect is one line, printing the address the pod's launcher dials", () => {
    expect(fn("pod_connect").split("\n")).toHaveLength(1);
    const pods = {
      connected: pod(),
      stray: pod(withConnect("tcp://192.0.2.9:13371")),
      earlier: renamed("architect"),
    };
    const { exitCode, out } = runScript(`root=${JSON.stringify(root)}
op() { jq -e --arg p "$3" '.[$p]' <<<${JSON.stringify(JSON.stringify(pods))}; }
${fn("pod_connect")}
printf '%s|%s|%s\\n' "$(pod_connect connected)" "$(pod_connect stray)" "$(pod_connect earlier)"
if absent=$(pod_connect absent); then echo "absent read: $absent"; fi`);
    expect(exitCode).toBe(0);
    expect(out).toBe(`${goldenStream}|tcp://192.0.2.9:13371|\n`);
  });

  test("every claim on the handed addresses passes, judged from one read of legion claims", () => {
    const { exitCode, out, reads } = judge({ pods: { "pod-a": movedPod() }, envs: handed, claims });
    expect(exitCode).toBe(0);
    expect(reads).toBe(1);
    expect(out).toContain(
      `claim-b: pod pod-a's launcher dials ${movedStream}; its tester is told LEGION_DAEMON_URL ${movedDaemonURL} and the run's services`
    );
    expect(out).toContain(
      "all 2 claims that run a process are on the addresses the daemon hands now"
    );
  });

  test("a pod whose launcher keeps the old --connect fails, naming the claim, role and pod", () => {
    const { exitCode, out } = judge({ pods: { "pod-a": pod() }, envs: handed, claims });
    expect(exitCode).toBe(1);
    expect(out).toEndWith(
      `FAIL: claim-a's architect in pod pod-a's launcher dials ${goldenStream}, not ${movedStream}\n`
    );
  });

  test("a pod with no roles container fails, naming no address", () => {
    const { exitCode, out } = judge({
      pods: { "pod-a": renamed("architect") },
      envs: handed,
      claims,
    });
    expect(exitCode).toBe(1);
    expect(out).toEndWith(
      `FAIL: claim-a's architect in pod pod-a's launcher dials nothing, not ${movedStream}\n`
    );
  });

  test("a role told the old API fails, naming it", () => {
    const envs = {
      ...handed,
      "pod-a/tester": handedEnv({ LEGION_DAEMON_URL: "http://192.0.2.250:13370" }),
    };
    const { exitCode, out } = judge({ pods: { "pod-a": movedPod() }, envs, claims });
    expect(exitCode).toBe(1);
    expect(out).toEndWith(
      `FAIL: claim-b's tester in pod pod-a has LEGION_DAEMON_URL=http://192.0.2.250:13370, want ${movedDaemonURL}\n`
    );
  });

  test("a role told another Envoy fails, naming the run's input, not the address", () => {
    const envs = { ...handed, "pod-a/architect": handedEnv({ ENVOY_URL: "https://192.0.2.9" }) };
    const { exitCode, out } = judge({ pods: { "pod-a": movedPod() }, envs, claims });
    expect(exitCode).toBe(1);
    expect(out).toBe(
      "FAIL: claim-a's architect in pod pod-a has ENVOY_URL differs from LEGION_E2E_ENVOY_URL\n"
    );
  });

  test("after the respelling, a role still told the old spelling fails, naming the respelled input", () => {
    const respelled = "https://ENVOY.INTERNAL.EXAMPLE:8443";
    const envs = { ...handed, "pod-a/architect": handedEnv({ ENVOY_URL: respelled }) };
    const { exitCode, out } = judge({
      pods: { "pod-a": movedPod() },
      envs,
      claims,
      handedEnvoy: respelled,
      envoySource: "LEGION_E2E_ENVOY_URL with its host upper-cased",
    });
    expect(exitCode).toBe(1);
    expect(out).not.toContain("internal.example");
    expect(out).toEndWith(
      "FAIL: claim-b's tester in pod pod-a has ENVOY_URL differs from LEGION_E2E_ENVOY_URL with its host upper-cased\n"
    );
  });

  test("a pod the operator cannot read fails, naming the pod", () => {
    const { exitCode, out } = judge({
      pods: { "pod-a": movedPod() },
      envs: handed,
      claims: [...claims, { token: "claim-c", pod: "pod-c", role: "planner" }],
    });
    expect(exitCode).toBe(1);
    expect(out).toEndWith("FAIL: the operator could not read pod pod-c\n");
  });

  test("a role with no readable Oh My Pi fails, naming its role's namespaces", () => {
    const { exitCode, out } = judge({
      pods: { "pod-a": movedPod() },
      envs: { "pod-a/architect": handedEnv() },
      claims,
    });
    expect(exitCode).toBe(1);
    expect(out).toContain(
      "FAIL: claim-b's tester in pod pod-a has no readable Oh My Pi environment in its tester namespaces"
    );
  });

  test("a legion claims read that fails fails the check, judging nothing", () => {
    const { exitCode, out, reads } = judge({
      pods: { "pod-a": movedPod() },
      envs: handed,
      claims: "fails",
    });
    expect(exitCode).toBe(1);
    expect(reads).toBe(1);
    expect(out).toBe("FAIL: legion claims could not be read\n");
  });

  test("a read that shows no claim running a process fails rather than passing empty", () => {
    const { exitCode, out } = judge({ pods: {}, envs: {}, claims: [] });
    expect(exitCode).toBe(1);
    expect(out).toBe("FAIL: legion claims shows no claim that runs a process\n");
  });

  test("upper_host upper-cases the authority alone", () => {
    const { out } = runScript(`${fn("upper_host")}
upper_host ${envoy}
upper_host http://envoy.internal.example`);
    expect(out).toBe("https://ENVOY.INTERNAL.EXAMPLE:8443\nhttp://ENVOY.INTERNAL.EXAMPLE\n");
  });

  test("scrub hides a production host however it is spelled", () => {
    const { out } =
      runScript(`service_hosts=(dispatch.internal.example envoy.internal.example:8443 nats.internal.example gateway.internal.example)
${fn("scrub")}
printf 'dial https://ENVOY.INTERNAL.EXAMPLE:8443: refused; envoy.internal.example too\\n' | scrub`);
    expect(out).toBe(
      "dial https://<LEGION_E2E_ENVOY_URL>:8443: refused; <LEGION_E2E_ENVOY_URL> too\n"
    );
  });

  describe("claims_relaunched", () => {
    const claim = (token: string, generation: number, uid = "uid-1") => ({
      token,
      generation,
      incarnation: `${uid}/${generation}`,
    });
    const relaunched = (before: object[], after: object[]) =>
      JSON.parse(
        runScript(`${fn("claims_relaunched")}
claims_relaunched ${JSON.stringify(JSON.stringify(before))} ${JSON.stringify(JSON.stringify(after))}`)
          .out
      );
    const before = [claim("a", 1), claim("b", 3)];

    test("lists nothing when every claim runs as it did", () => {
      expect(relaunched(before, before)).toEqual([]);
    });

    test("lists a claim at its next generation and one in another pod, with what each was", () => {
      expect(relaunched(before, [claim("a", 2), claim("b", 3, "uid-2")])).toEqual([
        {
          token: "a",
          generation: 2,
          incarnation: "uid-1/2",
          was: { generation: 1, incarnation: "uid-1/1" },
        },
        {
          token: "b",
          generation: 3,
          incarnation: "uid-2/3",
          was: { generation: 3, incarnation: "uid-1/3" },
        },
      ]);
    });

    test("does not list a claim that runs no process now", () => {
      expect(relaunched(before, [claim("a", 1)])).toEqual([]);
    });
  });

  test("pod_resume prints the one --resume word the role's processes carry, or nothing", () => {
    const { out } = runScript(`pod_commands() {
  case "$2" in
    tester) printf '%s\\n' "legion launcher --role tester" "legion worker-shim -- omp --resume=/s/a.jsonl --mode rpc" "omp --resume=/s/a.jsonl --mode rpc" ;;
    planner) printf '%s\\n' "legion launcher --role planner" ;;
  esac
}
${fn("pod_resume")}
printf '%s|%s\\n' "$(pod_resume pod tester)" "$(pod_resume pod planner)"`);
    expect(out).toBe("--resume=/s/a.jsonl|\n");
  });
});

describe("stage4b-pods.jq's ready_pods", () => {
  // event is one pod watch line: a pod with these labels whose containers CONTAINERS are ready,
  // every one but the one NOT_READY names.
  function event(name: string, labels: object, containers = ["roles"], notReady?: string) {
    return JSON.stringify({
      kind: "Pod",
      object: {
        kind: "Pod",
        metadata: { name, labels },
        status: {
          containerStatuses: containers.map((c) => ({ name: c, ready: c !== notReady })),
        },
      },
    });
  }

  test("keeps an issue pod whose roles container is ready, and leaves out the probe, a control, an unready pod and one of the earlier layout", () => {
    const watch = [
      event("kept", {}),
      event("probe", { "legion.dev/probe": "x" }),
      event("control", { "legion.dev/e2e-control": "x" }),
      event("unready", {}, ["roles"], "roles"),
      event("earlier", {}, roles),
    ].join("\n");
    expect(
      runJq(["-r", "-L", lib, 'include "stage4b-pods"; ready_pods | .metadata.name'], watch)
    ).toBe("kept\n");
  });
});

// The pod-shape checkpoint's negative control (wrong_runtime_control) appends one line to the pod
// watch: the last ready Sandbox pod's event with its runtimeClassName set to runc. pod_shape_verdict
// reads watch events, {type, object}, so the control holds only if that line is one and the verdict
// then names the pod's runtime class; a bare pod object is dropped unread and the verdict stays as
// the real record left it.
describe("the pod-shape checkpoint's wrong-runtime control", () => {
  // watchEvent is one pod watch line for the golden root issue pod, uid UID, its roles container
  // ready, as `kubectl get pods --watch -o json --output-watch-events` writes it.
  function watchEvent(uid: string) {
    const object = {
      kind: "Pod",
      metadata: { name: `pod-${uid}`, uid, creationTimestamp: created, labels: {} },
      spec: pod().spec,
      status: { containerStatuses: [{ name: "roles", ready: true }] },
    };
    return JSON.stringify({ type: "MODIFIED", object });
  }
  // verdict runs wrong_runtime_control on WATCH, appends its line as the checkpoint does, and runs
  // pod_shape_verdict on the result; it also runs the verdict on WATCH alone, the baseline. Each
  // keeps only the lines naming a runtime class: the golden pod departs from the run's other rules,
  // which are not under test here.
  function verdict(watch: string[]) {
    const evidence = join(dir, `run-${++runs}`);
    mkdirSync(evidence);
    writeFileSync(
      join(evidence, "worker-streams.jsonl"),
      `${JSON.stringify({ since: "2026-10-06T11:00:00Z", stream: goldenStream })}\n`
    );
    writeFileSync(join(evidence, "pod-watch.json"), watch.map((line) => `${line}\n`).join(""));
    const out =
      runScript(`root=${JSON.stringify(root)} evidence=${JSON.stringify(evidence)} work=${JSON.stringify(evidence)} route_configmap=route gateway_audience=audience
run_resources=${JSON.stringify(JSON.stringify(defaults))}
${shapeProblems}
${fn("pod_shape_verdict")}
${fn("wrong_runtime_control")}
wrong_runtime_control "$evidence/pod-watch.json" >"$work/wrong-shape.json"
cat "$evidence/pod-watch.json" "$work/wrong-shape.json" >"$work/pod-watch-wrong-shape.json"
printf 'control line keys %s\\n' "$(jq -c keys "$work/wrong-shape.json")"
pod_shape_verdict "$evidence/pod-watch.json" | sed 's/^/baseline /'
pod_shape_verdict "$work/pod-watch-wrong-shape.json" | sed 's/^/control /'`);
    if (out.exitCode !== 0) throw new Error(`the control's run exited ${out.exitCode}`);
    const lines = out.out.split("\n");
    // Each verdict line is `<uid>: <problem>;<problem>;…`, and a problem may itself hold ": ".
    const runtime = (prefix: string) =>
      lines
        .filter((line) => line.startsWith(prefix))
        .map((line) => {
          const rest = line.slice(prefix.length);
          const at = rest.indexOf(": ");
          const problems = rest
            .slice(at + 2)
            .split(";")
            .filter((p) => p.startsWith("runtimeClassName"));
          return problems.length === 0 ? "" : `${rest.slice(0, at)}: ${problems.join(";")}`;
        })
        .filter((line) => line !== "");
    return {
      keys: lines
        .find((line) => line.startsWith("control line keys "))
        ?.slice("control line keys ".length),
      baseline: runtime("baseline "),
      control: runtime("control "),
    };
  }

  test("the verdict names the last ready pod's runtime class, and the line is a watch event", () => {
    const result = verdict([watchEvent("uid-1"), watchEvent("uid-2")]);
    expect(result.baseline).toEqual([]);
    expect(result.control).toEqual(["uid-2: runtimeClassName runc"]);
    expect(result.keys).toBe('["object","type"]');
  });
});

// The pod-watch-verdict checkpoint's matching (pod_watch_verdict, never_scheduled_deaths): the
// daemon names a dead process by its claim, whose last word is its role, and its incarnation,
// `<pod uid>/<generation>`; the driver records an action by pod uid, with the role for a kill
// (driver_action). A `kill-role` accounts for a death of the role it killed, any generation, in
// its pod and no other role's; a `delete-pod` or `close` for a death of any role in its pod. Neither
// accounts for another pod, a pod whose uid it is a prefix of included; a death whose pod the
// scheduler never placed is accounted for by that pod's own conditions; and a record line in any
// other form is named.
describe("the pod-watch-verdict checkpoint's matching", () => {
  const scheduled = (uid: string) => ({
    kind: "Pod",
    metadata: { name: `pod-${uid}`, uid, labels: {} },
    spec: { nodeName: "node-1" },
    status: { phase: "Running", conditions: [{ type: "PodScheduled", status: "True" }] },
  });
  const unscheduled = (uid: string) => ({
    kind: "Pod",
    metadata: { name: `pod-${uid}`, uid, labels: {} },
    spec: {},
    status: {
      phase: "Pending",
      conditions: [{ type: "PodScheduled", status: "False", reason: "Unschedulable" }],
    },
  });
  // The memory hog the verdict requires to have been seen OOMKilled.
  const hog = {
    kind: "Pod",
    metadata: { name: "hog", uid: "uid-hog", labels: { "legion.dev/e2e-control": "memory-hog" } },
    spec: {},
    status: {
      containerStatuses: [{ state: { terminated: { reason: "OOMKilled", exitCode: 137 } } }],
    },
  };
  // died is a death line of the ROLE claim at INCARNATION, as the daemon writes it.
  const died = ([incarnation, role]: [string, string]) =>
    JSON.stringify({
      msg: "supervise: process died",
      claim: `legion-acme-acme-1-${role}`,
      incarnation,
      observed: "gone",
      detail: "test",
    });

  // verdict runs pod_watch_verdict on a watch of the pods, the daemon's death lines for DEATHS
  // ([incarnation, role]), and the driver record ACTIONS (its lines, without their time), and prints
  // the incarnation of each death it names, then each other line it names, in order.
  function verdict(deaths: [string, string][], actions: string[]) {
    const evidence = join(dir, `run-${++runs}`);
    mkdirSync(evidence);
    const pods = [scheduled("uid-a"), scheduled("uid-a2"), unscheduled("uid-u"), hog];
    writeFileSync(
      join(evidence, "pod-watch.json"),
      pods.map((object) => `${JSON.stringify({ type: "ADDED", object })}\n`).join("")
    );
    writeFileSync(join(evidence, "daemon.log"), deaths.map((d) => `${died(d)}\n`).join(""));
    writeFileSync(
      join(evidence, "driver-actions.txt"),
      actions
        .map((line) => {
          const [kind, uid, ...role] = line.split(" ");
          return `${[kind, uid, "2026-10-08T14:00:00.000Z", ...role].join(" ")}\n`;
        })
        .join("")
    );
    const out =
      runScript(`root=${JSON.stringify(root)} work=${JSON.stringify(evidence)} lost_detail="the issue's volume was lost: "
${fn("never_scheduled_deaths")}
${fn("pod_watch_verdict")}
pod_watch_verdict "$work/pod-watch.json" "$work/driver-actions.txt" "$work/daemon.log" >/dev/null || true
sed -n 's/^incarnation \\([^ ]*\\) of .* died with no driver action.*/\\1/p; /^driver action /p' "$work/pod-watch-verdict.txt"`);
    if (out.exitCode !== 0) throw new Error(`the verdict's run exited ${out.exitCode}`);
    return out.out.split("\n").filter((line) => line !== "");
  }

  test("a kill accounts for its own role, any generation, in its pod alone", () => {
    expect(
      verdict(
        [
          ["uid-a/1", "implementer"],
          ["uid-a/3", "implementer"],
          ["uid-a2/1", "implementer"],
          ["uid-b/1", "implementer"],
        ],
        ["kill-role uid-a implementer"]
      )
    ).toEqual(["uid-a2/1", "uid-b/1"]);
  });

  test("a kill accounts for no other role in its pod", () => {
    expect(
      verdict(
        [
          ["uid-a/1", "implementer"],
          ["uid-a/1", "tester"],
        ],
        ["kill-role uid-a implementer"]
      )
    ).toEqual(["uid-a/1"]);
  });

  test("a pod's deletion or close accounts for every role in it", () => {
    const deaths: [string, string][] = [
      ["uid-a/1", "implementer"],
      ["uid-a/2", "tester"],
    ];
    expect(verdict(deaths, ["delete-pod uid-a"])).toEqual([]);
    expect(verdict(deaths, ["close uid-a"])).toEqual([]);
  });

  test("with no action, every death of a scheduled pod is named", () => {
    expect(
      verdict(
        [
          ["uid-a/1", "implementer"],
          ["uid-a/3", "implementer"],
        ],
        []
      )
    ).toEqual(["uid-a/1", "uid-a/3"]);
  });

  test("a death whose pod was never scheduled is accounted for, whatever its generation", () => {
    expect(verdict([["uid-u/2", "planner"]], [])).toEqual([]);
  });

  test("a record line in another form is named, a kill without its role included", () => {
    expect(verdict([], ["kill-role uid-a"])).toEqual([
      "driver action kill-role uid-a 2026-10-08T14:00:00.000Z is not kill-role UID TIME ROLE, delete-pod UID TIME or close UID TIME",
    ]);
  });

  // An unreadable line accounts for nothing: a death in its pod is named beside the line, whatever
  // the line's kind (an old kind — a bare kill, or the one-container-per-role layout's kill of a role
  // container — a deletion with a field too many, a kill with one too many).
  test("an unreadable line accounts for no death in its pod, and the verdict names both", () => {
    for (const line of [
      "kill uid-a",
      "kill-container uid-a tester",
      "delete-pod uid-a extra",
      "kill-role uid-a tester extra",
    ]) {
      const [kind, uid, ...rest] = line.split(" ");
      expect(verdict([["uid-a/1", "tester"]], [line])).toEqual([
        `driver action ${[kind, uid, "2026-10-08T14:00:00.000Z", ...rest].join(" ")} is not kill-role UID TIME ROLE, delete-pod UID TIME or close UID TIME`,
        "uid-a/1",
      ]);
    }
  });

  // The checkpoint's live control (sibling_death_line): from the run's own record, a death of
  // another role in the pod of the last kill no deletion or close also ended, which the verdict must
  // name.
  test("the sibling-death control is built on the last kill of a pod nothing else ended", () => {
    const evidence = join(dir, `run-${++runs}`);
    mkdirSync(evidence);
    writeFileSync(
      join(evidence, "driver-actions.txt"),
      [
        "kill-role uid-a 2026-10-08T14:00:00.000Z planner",
        "kill-role uid-b 2026-10-08T14:01:00.000Z architect",
        "kill-role uid-c 2026-10-08T14:02:00.000Z merger",
        "delete-pod uid-c 2026-10-08T14:03:00.000Z",
      ]
        .map((line) => `${line}\n`)
        .join("")
    );
    const out = runScript(`${fn("sibling_death_line")}
sibling_death_line ${JSON.stringify(join(evidence, "driver-actions.txt"))}`);
    expect(out.exitCode).toBe(0);
    expect(JSON.parse(out.out)).toMatchObject({
      msg: "supervise: process died",
      claim: "legion-e2e-control-planner",
      incarnation: "uid-b/1",
    });
  });
});

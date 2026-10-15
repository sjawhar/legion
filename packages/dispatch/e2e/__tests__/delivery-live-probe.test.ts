import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Server, ServerWebSocket } from "bun";

import after from "../../../contracts/fixtures/delivery-live/after.json";
import delta from "../../../contracts/fixtures/delivery-live/delta.json";
import error from "../../../contracts/fixtures/delivery-live/error.json";
import heartbeat from "../../../contracts/fixtures/delivery-live/heartbeat.json";
import snapshot from "../../../contracts/fixtures/delivery-live/snapshot.json";
import type {
  DeliveryLiveClientFrame,
  DeliveryLiveDelta,
  DeliveryLiveSnapshot,
  DeliveryLiveView,
  DeliveryTimelineResponse,
} from "../../web/src/api/types";
import type { ProbeCompareLine, ProbeHeld, ProbeLine } from "../delivery-live-probe";

// The probe runs as an agent runs it, a process of its own, against a stand-in for Dispatch that
// replays the frames Dispatch's own diff writes (packages/contracts/fixtures/delivery-live): a
// timeline snapshot, the delta of a deploy, a merge and two pull requests leaving the window, a
// heartbeat, an error, and the snapshot a page subscribing after the delta gets.

const held = snapshot as unknown as DeliveryLiveSnapshot;
const change = delta as unknown as DeliveryLiveDelta;
const next = after as unknown as DeliveryLiveSnapshot;
const heldBody = held.body as DeliveryTimelineResponse;
const nextBody = next.body as DeliveryTimelineResponse;

const TOKEN = "probe-token";
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const probe = fileURLToPath(new URL("../delivery-live-probe.ts", import.meta.url));
const view: DeliveryLiveView = {
  kind: "timeline",
  query: "repo=acme%2Fwidgets",
  window: { preset: 28 },
};
/** Each test starts a Bun process and loads the probe's imports; the box these run on is shared. */
const PROCESS_TEST_MS = 30_000;

// The test preload (web/src/__tests__/setup.ts) puts happy-dom's Response on globalThis, which
// Bun.serve does not take as an answer; Bun's own fetch still answers with Bun's.
const NativeResponse = (await Bun.fetch("data:,")).constructor as typeof Response;

interface Script {
  /** Answers the probe's `count`th subscribe (from 1) on `socket`, for its subscription `sub`. */
  readonly onSubscribe: (socket: ServerWebSocket<undefined>, sub: string, count: number) => void;
  /** Each GET route's body, by path. */
  readonly routes?: Readonly<Record<string, unknown>>;
}

interface FakeDispatch {
  readonly url: string;
  /** Every frame the probe sent, in order. */
  readonly received: readonly DeliveryLiveClientFrame[];
  /** Every GET route the probe read. */
  readonly reads: readonly URL[];
  /** The Authorization header of every upgrade the probe asked for. */
  readonly upgrades: readonly (string | null)[];
}

interface Run {
  readonly lines: readonly ProbeLine[];
  readonly stderr: readonly string[];
  /** Sends the probe `signal`, as a Ctrl-C does. */
  kill(signal: NodeJS.Signals): void;
  /** Resolves once `ready` holds; rejects if the probe exits first. */
  until(ready: () => boolean): Promise<void>;
  /** The exit code, once both output streams are read to their end. */
  exited(): Promise<number>;
}

const servers: Server<undefined>[] = [];
const kills: (() => void)[] = [];
const scratchDirs: string[] = [];
/** Woken whenever something a test waits on happens: a line from the probe, a frame the probe
 *  sent, or the probe's output ending. Each test waits on that event, never on a guessed time. */
const waiters = new Set<() => void>();
const wake = () => {
  for (const waiter of [...waiters]) waiter();
};

afterEach(() => {
  waiters.clear();
  for (const kill of kills.splice(0)) kill();
  for (const server of servers.splice(0)) server.stop(true);
  for (const dir of scratchDirs.splice(0)) rmSync(dir, { force: true, recursive: true });
});

function scratch(): string {
  const dir = mkdtempSync(join(tmpdir(), "delivery-live-probe-"));
  scratchDirs.push(dir);
  return dir;
}

/** `fixture` as Dispatch sends it to the subscription `sub`. */
const forSub = (fixture: object, sub: string) => JSON.stringify({ ...fixture, sub });

function fakeDispatch(script: Script): FakeDispatch {
  const received: DeliveryLiveClientFrame[] = [];
  const reads: URL[] = [];
  const upgrades: (string | null)[] = [];
  let subscribes = 0;
  const server = Bun.serve({
    fetch(request, srv) {
      const url = new URL(request.url);
      if (url.pathname === "/ws/delivery") {
        const authorization = request.headers.get("authorization");
        upgrades.push(authorization);
        if (authorization !== `Bearer ${TOKEN}`) {
          return NativeResponse.json(
            { code: "UNAUTHORIZED", error: "invalid bearer token" },
            { status: 401 }
          );
        }
        return srv.upgrade(request)
          ? undefined
          : new NativeResponse("upgrade failed", { status: 400 });
      }
      reads.push(url);
      const body = script.routes?.[url.pathname];
      return body === undefined
        ? NativeResponse.json({ code: "NOT_FOUND", error: url.pathname }, { status: 404 })
        : NativeResponse.json(body);
    },
    hostname: "127.0.0.1",
    port: 0,
    websocket: {
      message(socket, data) {
        // The probe's own frame, which the assertions below hold to the contract.
        const frame = JSON.parse(String(data)) as DeliveryLiveClientFrame;
        received.push(frame);
        if (frame.type === "subscribe") {
          subscribes += 1;
          script.onSubscribe(socket, frame.sub, subscribes);
        }
        wake();
      },
    },
  });
  servers.push(server);
  return { reads, received, upgrades, url: `http://127.0.0.1:${server.port}` };
}

async function readLines(
  stream: ReadableStream<Uint8Array>,
  onLine: (line: string) => void
): Promise<void> {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let pending = "";
  for (let chunk = await reader.read(); !chunk.done; chunk = await reader.read()) {
    pending += decoder.decode(chunk.value, { stream: true });
    const complete = pending.split("\n");
    pending = complete.pop() ?? "";
    for (const line of complete) if (line !== "") onLine(line);
  }
  if (pending !== "") onLine(pending);
}

function runProbe(args: readonly string[]): Run {
  const child = Bun.spawn([process.execPath, probe, ...args], {
    cwd: dispatchPackage,
    stderr: "pipe",
    stdin: "ignore",
    stdout: "pipe",
  });
  kills.push(() => child.kill("SIGKILL"));
  const lines: ProbeLine[] = [];
  const stderr: string[] = [];
  let finished = false;
  const reading = Promise.all([
    // The probe writes one JSON object per line; the assertions read each as the line it names.
    readLines(child.stdout, (line) => {
      lines.push(JSON.parse(line) as ProbeLine);
      wake();
    }),
    readLines(child.stderr, (line) => stderr.push(line)),
  ]).then(() => {
    finished = true;
    wake();
  });
  return {
    async exited() {
      await reading;
      return child.exited;
    },
    kill: (signal) => child.kill(signal),
    lines,
    stderr,
    until(ready) {
      const { promise, reject, resolve } = Promise.withResolvers<void>();
      const check = () => {
        if (ready()) {
          waiters.delete(check);
          resolve();
        } else if (finished) {
          waiters.delete(check);
          reject(new Error(`the probe ended first: ${JSON.stringify(lines)} ${stderr.join("\n")}`));
        }
      };
      waiters.add(check);
      check();
      return promise;
    },
  };
}

function args(dispatch: FakeDispatch, token = TOKEN, minutes = "1"): string[] {
  const tokenFile = join(scratch(), "token");
  writeFileSync(tokenFile, `${token}\n`, { mode: 0o600 });
  return [
    "--url",
    dispatch.url,
    "--token-file",
    tokenFile,
    "--view",
    JSON.stringify(view),
    "--minutes",
    minutes,
  ];
}

/** Each key of `record`'s maps, counted. */
const sizes = (record: Readonly<Record<string, object>>) =>
  Object.fromEntries(
    Object.entries(record).map(([key, value]) => [key, Object.keys(value).length])
  );
/** Each key of `record`'s maps, named. */
const names = (record: Readonly<Record<string, object>>) =>
  Object.fromEntries(Object.entries(record).map(([key, value]) => [key, Object.keys(value)]));
const ids = (items: readonly { readonly id: string | number }[]) => items.map((item) => item.id);
const comparisons = (lines: readonly ProbeLine[]) =>
  lines.filter((line): line is ProbeCompareLine => line.type === "compare");
const nothing = {
  changed_at: null,
  counts: null,
  maps: null,
  prs: null,
  runs: null,
  seq: null,
  session_groups: null,
  set: null,
};

test(
  "the probe writes one line per frame and holds what the page would after them",
  async () => {
    const sent: string[] = [];
    const dispatch = fakeDispatch({
      onSubscribe(socket, sub) {
        for (const text of [
          forSub(snapshot, sub),
          forSub(delta, sub),
          JSON.stringify(heartbeat),
          forSub(error, sub),
        ]) {
          sent.push(text);
          socket.send(text);
        }
      },
      routes: {
        "/api/v1/delivery/measures": nextBody.measures,
        "/api/v1/delivery/timeline": nextBody,
      },
    });
    const bodyFile = join(scratch(), "body.json");
    const run = runProbe([...args(dispatch), "--compare", "--body-file", bodyFile]);
    await run.until(() => run.lines.some((line) => line.type === "error"));
    run.kill("SIGINT");
    expect(await run.exited()).toBe(0);

    expect(dispatch.upgrades).toEqual([`Bearer ${TOKEN}`]);
    expect(dispatch.received).toEqual([{ sub: "probe", type: "subscribe", view }]);
    const bytes = sent.map((text) => Buffer.byteLength(text));
    const prsChange = change.items.prs;
    const runsChange = change.items.runs;
    if (prsChange === undefined || runsChange === undefined) {
      throw new Error("delta.json changes the pull requests and the runs");
    }
    expect(
      run.lines.filter((line) => line.type !== "compare").map(({ at: _at, ...line }) => line)
    ).toEqual([
      { resume: null, type: "open" },
      {
        ...nothing,
        bytes: bytes[0],
        changed_at: held.changed_at,
        counts: {
          color_counts: sizes(heldBody.color_counts),
          facet_counts: sizes(heldBody.facet_counts),
        },
        maps: {
          components: Object.keys(heldBody.components).length,
          issue_titles: Object.keys(heldBody.issue_titles).length,
          session_pr_counts: Object.keys(heldBody.session_pr_counts).length,
          session_titles: Object.keys(heldBody.session_titles).length,
        },
        prs: heldBody.prs.length,
        runs: heldBody.runs.length,
        seq: held.seq,
        session_groups: heldBody.session_groups.length,
        type: "snapshot",
      },
      {
        ...nothing,
        bytes: bytes[1],
        changed_at: change.changed_at,
        counts: {
          color_counts: names(change.counts.color_counts ?? {}),
          facet_counts: names(change.counts.facet_counts ?? {}),
        },
        maps: names(change.maps),
        prs: { remove: prsChange.remove, upsert: ids(prsChange.upsert) },
        runs: { remove: runsChange.remove, upsert: ids(runsChange.upsert) },
        seq: change.seq,
        set: Object.keys(change.set),
        type: "delta",
      },
      { ...nothing, bytes: bytes[2], type: "heartbeat" },
      {
        ...nothing,
        bytes: bytes[3],
        code: "DELIVERY_NOT_CONFIGURED",
        message: error.message,
        type: "error",
      },
    ]);
    // The delta names the deploy's run, the merge and the two pull requests that left.
    expect(prsChange.remove).toEqual(["acme/widgets#1", "acme/widgets#2"]);
    expect(ids(runsChange.upsert)).toEqual([42]);

    // What the probe holds is the snapshot a page subscribing afterwards gets. A delta never
    // carries freshness, which has its own frame, so it is the one field the two may differ in.
    const applied = (await Bun.file(bodyFile).json()) as ProbeHeld;
    const { freshness: _applied, ...appliedBody } = applied.body as DeliveryTimelineResponse;
    const { freshness: _next, ...expectedBody } = nextBody;
    expect(appliedBody).toEqual(expectedBody);
    expect(applied.revs).toEqual(next.revs);
    expect(applied.seq).toBe(change.seq);
    expect(applied.changed_at).toBe(change.changed_at);

    // --compare reads both routes for the window the delta moved to, with the view's own query.
    expect(
      dispatch.reads.map((url) => [
        url.pathname,
        url.searchParams.get("from"),
        url.searchParams.get("to"),
        url.searchParams.get("repo"),
      ])
    ).toEqual([
      ["/api/v1/delivery/timeline", nextBody.window.from, nextBody.window.to, "acme/widgets"],
      ["/api/v1/delivery/measures", nextBody.window.from, nextBody.window.to, "acme/widgets"],
    ]);
    const same = { equal: true, extra: [], missing: [] };
    const allEqual = { differ: [], equal: true };
    expect(
      comparisons(run.lines).map(({ at: _at, route, ...line }) => ({
        ...line,
        route: route.split("?")[0],
      }))
    ).toEqual([
      {
        equal: true,
        measures: allEqual,
        prs: same,
        route: "/api/v1/delivery/timeline",
        runs: same,
        status: 200,
        type: "compare",
      },
      {
        equal: true,
        measures: allEqual,
        prs: null,
        route: "/api/v1/delivery/measures",
        runs: null,
        status: 200,
        type: "compare",
      },
    ]);
  },
  PROCESS_TEST_MS
);

test(
  "--compare names each pull request, run and measure a GET answer differs in, and exits 1",
  async () => {
    const [firstPR] = nextBody.prs;
    const [firstRun] = nextBody.runs;
    if (firstPR === undefined || firstRun === undefined) {
      throw new Error("after.json holds a pull request and a run");
    }
    const dispatch = fakeDispatch({
      onSubscribe(socket, sub) {
        socket.send(forSub(snapshot, sub));
        socket.send(forSub(delta, sub));
      },
      routes: {
        "/api/v1/delivery/measures": nextBody.measures,
        "/api/v1/delivery/timeline": {
          ...nextBody,
          measures: {
            ...nextBody.measures,
            measures: { ...nextBody.measures.measures, rework_share: 0.99 },
          },
          prs: nextBody.prs.slice(1),
          runs: [...nextBody.runs, { ...firstRun, id: 99 }],
        },
      },
    });
    const run = runProbe([...args(dispatch), "--compare"]);
    await run.until(() => run.lines.some((line) => line.type === "delta"));
    run.kill("SIGINT");
    expect(await run.exited()).toBe(1);

    expect(
      comparisons(run.lines).map(({ equal, measures, prs, runs }) => ({
        equal,
        measures,
        prs,
        runs,
      }))
    ).toEqual([
      {
        equal: false,
        measures: { differ: ["measures.rework_share"], equal: false },
        prs: { equal: false, extra: [firstPR.id], missing: [] },
        runs: { equal: false, extra: [], missing: ["99"] },
      },
      { equal: true, measures: { differ: [], equal: true }, prs: null, runs: null },
    ]);
  },
  PROCESS_TEST_MS
);

test(
  "after a restart close the probe reconnects at once and resumes with the change and revisions it holds",
  async () => {
    const dispatch = fakeDispatch({
      onSubscribe(socket, sub, count) {
        if (count > 1) return;
        socket.send(forSub(snapshot, sub));
        socket.send(forSub(delta, sub));
        socket.close(1012, "service restart");
      },
    });
    const run = runProbe(args(dispatch));
    await run.until(() => dispatch.received.length === 2);
    run.kill("SIGINT");
    expect(await run.exited()).toBe(0);

    expect(dispatch.received).toEqual([
      { sub: "probe", type: "subscribe", view },
      { resume: { revs: next.revs, seq: change.seq }, sub: "probe", type: "subscribe", view },
    ]);
    expect(
      run.lines.map((line) => {
        if (line.type === "open") return `open ${line.resume}`;
        if (line.type === "close") return `close ${line.code}`;
        return line.type;
      })
    ).toEqual(["open null", "snapshot", "delta", "close 1012", `open ${change.seq}`]);
  },
  PROCESS_TEST_MS
);

test(
  "a refused upgrade ends the probe with exit 1, naming the refusal",
  async () => {
    const dispatch = fakeDispatch({ onSubscribe() {} });
    const run = runProbe(args(dispatch, "not-the-token"));
    expect(await run.exited()).toBe(1);
    expect(dispatch.upgrades).toEqual(["Bearer not-the-token"]);
    expect(run.stderr.join("\n")).toContain("Dispatch refused the connection to ws://127.0.0.1:");
  },
  PROCESS_TEST_MS
);

test(
  "a refused view ends the watch with exit 1, naming the code",
  async () => {
    const dispatch = fakeDispatch({
      onSubscribe(socket, sub) {
        socket.send(
          JSON.stringify({
            code: "INVALID_QUERY",
            message: "window is wider than 84 days",
            sub,
            type: "error",
          })
        );
      },
    });
    const run = runProbe(args(dispatch));
    expect(await run.exited()).toBe(1);
    expect(run.stderr.join("\n")).toContain(
      "Dispatch refused the view: INVALID_QUERY: window is wider than 84 days"
    );
  },
  PROCESS_TEST_MS
);

test(
  "the watch ends on its own after --minutes",
  async () => {
    const dispatch = fakeDispatch({ onSubscribe() {} });
    const started = Date.now();
    // 0.01 minutes is 600 ms of the probe's own clock, in another process, which no fake timer
    // here reaches: the test waits for that process to end and checks it ended no sooner.
    const run = runProbe(args(dispatch, TOKEN, "0.01"));
    expect(await run.exited()).toBe(0);
    expect(Date.now() - started).toBeGreaterThanOrEqual(600);
    expect(dispatch.received).toEqual([{ sub: "probe", type: "subscribe", view }]);
  },
  PROCESS_TEST_MS
);

test(
  "a missing flag is a usage error, exit 2",
  async () => {
    const dispatch = fakeDispatch({ onSubscribe() {} });
    const run = runProbe(args(dispatch).slice(0, 4));
    expect(await run.exited()).toBe(2);
    expect(run.stderr.join("\n")).toContain("--minutes is required");
    expect(dispatch.upgrades).toEqual([]);
  },
  PROCESS_TEST_MS
);

import { afterAll, afterEach, beforeEach, expect, test } from "bun:test";
import { mkdirSync, readdirSync, readFileSync, readlinkSync, rmSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";
import { removeTemporaryDirectories, temporaryDirectory } from "../../test/temp-dir";
import {
  askgateState,
  pidNamespace,
  processStart,
  selfOwner,
  writeAskgateRecord,
} from "../askgate-marker";

afterAll(removeTemporaryDirectories);

// Two Oh My Pi processes can bind one session id at the same time (the same session resumed
// twice). Each one stays an owner of the session's AskGate record: a write by one never drops the
// other, or the CLI would refuse the dropped owner's literal writes until it binds again.

const MARKER_MODULE = join(import.meta.dir, "..", "askgate-marker.ts");
const WRITERS = 4;
// Writes for each writer. Enough to catch a dropped owner: at this count the row still fails a
// writer that drops the others, and more writes only make its cost track the machine's speed.
const WRITES = 200;

/** The children the writers row below spawns, ended here when its own cleanup was lost to the
 * timeout. Bun runs `afterEach` hooks in the order they are registered and skips the rest once one
 * throws, so this hook is registered before the one that removes the temporary tree: it runs first,
 * and a throw while removing the tree cannot leave a child running. */
const spawned: Bun.Subprocess[] = [];
afterEach(() => {
  for (const child of spawned) child.kill();
  spawned.length = 0;
});

let root = "";
beforeEach(() => {
  root = temporaryDirectory("askgate-record-");
});
afterEach(() => rmSync(root, { recursive: true, force: true }));

/** Reads a child's output one line at a time while the child keeps running. */
function lineReader(stream: ReadableStream<Uint8Array>): () => Promise<string> {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let buffered = "";
  return async () => {
    while (!buffered.includes("\n")) {
      const { value, done } = await reader.read();
      if (done) throw new Error(`the child ended before a full line: ${buffered}`);
      buffered += decoder.decode(value, { stream: true });
    }
    const end = buffered.indexOf("\n");
    const line = buffered.slice(0, end);
    buffered = buffered.slice(end + 1);
    return line;
  };
}

test("processes that bind one session id at the same time each stay an owner of its record", async () => {
  const dir = join(root, "sessions", "ses-1");
  // Each writer says it is ready, waits for the parent's line, binds the session again and again,
  // and after each write reads the record: for its own store, and, once it has read every writer's
  // store there, for a store that went. Each one stays live until its stdin closes, so no owner of
  // a full record can legitimately go, and a store that went is one a write dropped.
  const writer = join(root, "writer.ts");
  writeFileSync(
    writer,
    `import { askgateState, openStore, selfOwner, writeAskgateRecord } from ${JSON.stringify(MARKER_MODULE)};
const dir = ${JSON.stringify(dir)};
const owner = selfOwner(${JSON.stringify(root)});
openStore(owner.store);
const stdin = Bun.stdin.stream().getReader();
console.log("ready");
await stdin.read();
let dropped = 0;
let lost = 0;
let full = false;
for (let write = 0; write < ${WRITES}; write += 1) {
  writeAskgateRecord(dir, owner, "warn");
  const state = askgateState(dir);
  const stores = state.kind === "on" ? state.stores : [];
  if (!stores.includes(owner.store)) dropped += 1;
  if (stores.length >= ${WRITERS}) full = true;
  else if (full) lost += 1;
}
console.log(JSON.stringify({ store: owner.store, dropped, lost }));
while (!(await stdin.read()).done);
`
  );
  const children = Array.from({ length: WRITERS }, () =>
    Bun.spawn([process.execPath, writer], { stdin: "pipe", stdout: "pipe", stderr: "inherit" })
  );
  spawned.push(...children);
  try {
    const lines = children.map((child) => lineReader(child.stdout));
    expect(await Promise.all(lines.map((next) => next()))).toEqual(Array(WRITERS).fill("ready"));
    for (const child of children) {
      child.stdin.write("go\n");
      child.stdin.flush();
    }
    const reports = (await Promise.all(lines.map((next) => next()))).map(
      (line) => JSON.parse(line) as { store: string; dropped: number; lost: number }
    );
    expect(reports.map((report) => report.dropped)).toEqual(Array(WRITERS).fill(0));
    expect(reports.map((report) => report.lost)).toEqual(Array(WRITERS).fill(0));
    const state = askgateState(dir);
    expect(state.kind === "on" ? [...state.stores].sort() : state).toEqual(
      reports.map((report) => report.store).sort()
    );
  } finally {
    for (const child of children) child.stdin.end();
    await Promise.all(children.map((child) => child.exited));
  }
  // The limit is a hang guard, not a margin for a slow machine: a child that never prints its
  // line, or a renameSync or readdirSync that never returns.
}, 60_000);

// A pid and a start time name a process only in the pid namespace they were read in, and an agent
// box is a namespace of its own over the host's state directory. The owner carries its namespace in
// its file and in its store's name, so a reader of another namespace knows it cannot look it up,
// and two boxes' processes with one pid and start time never share a store.
test("an owner names its pid namespace in its record and in its store's name", () => {
  const ns = /^pid:\[([0-9]+)\]$/.exec(readlinkSync("/proc/self/ns/pid"))?.[1];
  expect(pidNamespace()).toBe(ns ?? null);
  const owner = selfOwner(root);
  const start = processStart(process.pid);
  expect(owner).toMatchObject({ pid: process.pid, start, ns });
  expect(basename(owner.store)).toBe(`${process.pid}-${start}-${ns}`);
  const dir = join(root, "sessions", "ses-1");
  writeAskgateRecord(dir, owner, "block");
  expect(readdirSync(join(dir, "askgate"))).toEqual([`${process.pid}-${start}-${ns}.json`]);
  expect(
    JSON.parse(readFileSync(join(dir, "askgate", `${process.pid}-${start}-${ns}.json`), "utf8"))
  ).toEqual({ v: 2, pid: process.pid, start, ns, store: owner.store, mode: "block" });
  expect(askgateState(dir)).toEqual({ kind: "on", stores: [owner.store], mode: "block" });
});

// A bind removes another owner's file only when that owner is known gone: one of this pid
// namespace whose pid no longer has its start time. A file of another namespace, or one an earlier
// pi-envoy wrote with no namespace, may name a live process this one cannot look up, which would
// lose its record until its next call, so it stays.
test("a bind removes only a gone owner of its own pid namespace from the record", () => {
  const dir = join(root, "sessions", "ses-1");
  const record = join(dir, "askgate");
  const ns = pidNamespace();
  const other = (name: string, owner: Record<string, unknown>) => {
    mkdirSync(record, { recursive: true });
    writeFileSync(join(record, `${name}.json`), JSON.stringify({ v: 2, ...owner, mode: "block" }));
  };
  const store = join(root, "askgate", "gone");
  other("same-ns", { pid: process.pid, start: "1", ns, store });
  other("other-ns", { pid: process.pid, start: "1", ns: "1", store });
  other("no-ns", { pid: process.pid, start: "1", store });
  writeAskgateRecord(dir, selfOwner(root), "warn");
  expect(readdirSync(record).sort()).toEqual(
    [`${basename(selfOwner(root).store)}.json`, "no-ns.json", "other-ns.json"].sort()
  );
});

import { afterEach, beforeEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { askgateState } from "../askgate-marker";

// Two Oh My Pi processes can bind one session id at the same time (the same session resumed
// twice). Each one stays an owner of the session's AskGate record: a write by one never drops the
// other, or the CLI would refuse the dropped owner's literal writes until it binds again.

const MARKER_MODULE = join(import.meta.dir, "..", "askgate-marker.ts");
const WRITERS = 4;
const WRITES = 2_000;

let root = "";
beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), "askgate-record-"));
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
  // and after each write reads the record for its own store. It then stays live until its stdin
  // closes, so the record's final state is read with every owner live.
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
for (let write = 0; write < ${WRITES}; write += 1) {
  writeAskgateRecord(dir, owner);
  const state = askgateState(dir);
  if (state.kind !== "on" || !state.stores.includes(owner.store)) dropped += 1;
}
console.log(JSON.stringify({ store: owner.store, dropped }));
while (!(await stdin.read()).done);
`
  );
  const children = Array.from({ length: WRITERS }, () =>
    Bun.spawn([process.execPath, writer], { stdin: "pipe", stdout: "pipe", stderr: "inherit" })
  );
  try {
    const lines = children.map((child) => lineReader(child.stdout));
    expect(await Promise.all(lines.map((next) => next()))).toEqual(Array(WRITERS).fill("ready"));
    for (const child of children) {
      child.stdin.write("go\n");
      child.stdin.flush();
    }
    const reports = (await Promise.all(lines.map((next) => next()))).map(
      (line) => JSON.parse(line) as { store: string; dropped: number }
    );
    expect(reports.map((report) => report.dropped)).toEqual(Array(WRITERS).fill(0));
    const state = askgateState(dir);
    expect(state.kind === "on" ? [...state.stores].sort() : state).toEqual(
      reports.map((report) => report.store).sort()
    );
  } finally {
    for (const child of children) child.stdin.end();
    await Promise.all(children.map((child) => child.exited));
  }
}, 30_000);

import { afterEach, describe, expect, it, jest } from "bun:test";
import type { ExecFn } from "../dispatch-cwd";
import { readTmuxPlace, type TmuxPlace } from "../tmux-place";

const SOCKET = "/tmp/tmux-1000/default";
const SERVER_PID = "4242";
const PANE = "%3";
const ENV = { TMUX: `${SOCKET},${SERVER_PID},0`, TMUX_PANE: PANE };

/** The bound the reader holds its one tmux call to (dispatch-cwd.ts's TMUX_PANE_LOOKUP_MS). */
const PANE_BOUND_MS = 1_000;

/** The format the reader asks for, written out here so a change to the fields or their order has
 *  to be made twice: the listener reads these ten in this order. */
const FORMAT = [
  "#{session_name}",
  "#{window_id}",
  "#{window_index}",
  "#{window_name}",
  "#{pane_id}",
  "#{pane_index}",
  "#{pid}",
  "#{pane_pid}",
  "#{window_layout}",
  "#{window_zoomed_flag}",
].join("\t");

/** The one call a reading may make, as `fakeExec` keys it. */
function callOn(socket: string): string {
  return `tmux -u -S ${socket} display-message -p -t ${PANE} ${FORMAT}`;
}

const CALL = callOn(SOCKET);

/** One answer's ten fields, as strings, before tmux joins them with tabs. */
type Fields = {
  sessionName: string;
  windowId: string;
  windowIndex: string;
  windowName: string;
  paneId: string;
  paneIndex: string;
  pid: string;
  panePid: string;
  layout: string;
  zoomed: string;
};

// The pane's first process is this process: the reader takes a pane only when its process is
// this one or one of its ancestors, and a test cannot know a pane process it did not start.
const ANSWER: Fields = {
  sessionName: "legion",
  windowId: "@7",
  windowIndex: "3",
  windowName: "worker",
  paneId: PANE,
  paneIndex: "1",
  pid: SERVER_PID,
  panePid: String(process.pid),
  layout: "b2f1,208x52,0,0,5",
  zoomed: "0",
};

/** What `display-message -p` prints for `FORMAT`: the fields tab-joined, one trailing newline. */
function answer(changes: Partial<Fields> = {}): string {
  const fields = { ...ANSWER, ...changes };
  return `${[
    fields.sessionName,
    fields.windowId,
    fields.windowIndex,
    fields.windowName,
    fields.paneId,
    fields.paneIndex,
    fields.pid,
    fields.panePid,
    fields.layout,
    fields.zoomed,
  ].join("\t")}\n`;
}

const PLACE: TmuxPlace = {
  socket: SOCKET,
  server_pid: SERVER_PID,
  session_name: "legion",
  window_id: "@7",
  window_index: 3,
  window_name: "worker",
  pane_id: PANE,
  pane_index: 1,
  layout: "b2f1,208x52,0,0,5",
  zoomed: false,
};

// The bounded tests drive time with fake timers and check that a reading has settled before they
// await it: Bun's test timeout does not fire while fake timers are on, so awaiting a reading that
// never settles would hang the run instead of failing it.
afterEach(() => {
  jest.useRealTimers();
});

/** Run every job already queued: a settled race reaches its caller's `.finally` within these. */
async function settle(): Promise<void> {
  for (let turn = 0; turn < 50; turn += 1) await Promise.resolve();
}

/** Route each call by `${file} ${args.join(" ")}` to a canned stdout, or throw for one nothing
 *  scripted, which is how a tmux client that exits non-zero reaches the reader. */
function fakeExec(script: Record<string, string>): { exec: ExecFn; calls: string[] } {
  const calls: string[] = [];
  const exec: ExecFn = async (file, args) => {
    const key = [file, ...args].join(" ");
    calls.push(key);
    if (!(key in script)) throw new Error(`unscripted command: ${key}`);
    return { stdout: script[key] };
  };
  return { exec, calls };
}

/** A tmux stand-in, run under fake timers, that answers after `answerAfterMs` or never (null).
 *  It never heeds its abort signal, as a tmux client waiting to connect to a busy server catches
 *  the SIGTERM a plain execFile timeout sends and keeps waiting. */
function slowTmux(answerAfterMs: number | null, stdout: string = answer()) {
  const seen: { calls: string[]; signal?: AbortSignal } = { calls: [] };
  const exec: ExecFn = (file, args, options) => {
    seen.calls.push([file, ...args].join(" "));
    seen.signal = options.signal;
    const pending = Promise.withResolvers<{ stdout: string }>();
    if (answerAfterMs !== null) setTimeout(() => pending.resolve({ stdout }), answerAfterMs);
    return pending.promise;
  };
  return { exec, seen };
}

describe("readTmuxPlace", () => {
  it("reads the pane's place from one display-message call", async () => {
    const { exec, calls } = fakeExec({ [CALL]: answer() });
    expect(await readTmuxPlace(ENV, exec)).toEqual({ kind: "place", place: PLACE });
    expect(calls).toEqual([CALL]);
  });

  it("spawns the tmux call in a directory no session can delete under it", async () => {
    const directories: string[] = [];
    const exec: ExecFn = async (_file, _args, options) => {
      directories.push(options.cwd);
      return { stdout: answer() };
    };
    // tmux reads nothing from the directory — the socket `TMUX` names is absolute — while a
    // directory removed under a live session makes the spawn fail, and `tryExec` reports that
    // failure as `refused`, which would clear the place of a session still in its pane.
    await readTmuxPlace(ENV, exec);

    expect(directories).toEqual(["/"]);
  });

  it("reads a zoomed pane's flag as a boolean", async () => {
    const { exec, calls } = fakeExec({ [CALL]: answer({ zoomed: "1" }) });
    expect(await readTmuxPlace(ENV, exec)).toEqual({
      kind: "place",
      place: { ...PLACE, zoomed: true },
    });
    expect(calls).toEqual([CALL]);
  });

  it("runs nothing at all outside tmux, with either variable missing", async () => {
    for (const env of [{}, { TMUX: ENV.TMUX }, { TMUX_PANE: PANE }]) {
      const { exec, calls } = fakeExec({ [CALL]: answer() });
      expect(await readTmuxPlace(env, exec)).toEqual({ kind: "refused" });
      expect(calls).toEqual([]);
    }
  });

  it("reads the whole socket path from a TMUX whose socket holds a comma", async () => {
    const socket = "/tmp/odd,place/default";
    const call = callOn(socket);
    const { exec, calls } = fakeExec({ [call]: answer() });
    expect(
      await readTmuxPlace({ TMUX: `${socket},${SERVER_PID},0`, TMUX_PANE: PANE }, exec)
    ).toEqual({ kind: "place", place: { ...PLACE, socket } });
    expect(calls).toEqual([call]);
  });

  it("reports no place when another server now answers on the socket", async () => {
    const { exec, calls } = fakeExec({ [CALL]: answer({ pid: "9999" }) });
    expect(await readTmuxPlace(ENV, exec)).toEqual({ kind: "refused" });
    expect(calls).toEqual([CALL]);
  });

  it("reports no place for a pane that is not this session's", async () => {
    const child = Bun.spawnSync([
      process.execPath,
      "-e",
      "process.stdout.write(String(process.pid))",
    ]);
    for (const panePid of ["1", child.stdout.toString()]) {
      const { exec, calls } = fakeExec({ [CALL]: answer({ panePid }) });
      expect(await readTmuxPlace(ENV, exec)).toEqual({ kind: "refused" });
      expect(calls).toEqual([CALL]);
    }
  });

  it("reports no place when the call is refused or prints nothing usable", async () => {
    // Nothing scripted: the exec rejects, as execFile does for a tmux that exits non-zero.
    const refused = fakeExec({});
    expect(await readTmuxPlace(ENV, refused.exec)).toEqual({ kind: "refused" });
    expect(refused.calls).toEqual([CALL]);

    for (const stdout of ["", "\n", "legion\t@7\t3\n", `${answer().trim()}\textra\n`]) {
      const { exec, calls } = fakeExec({ [CALL]: stdout });
      expect(await readTmuxPlace(ENV, exec)).toEqual({ kind: "refused" });
      expect(calls).toEqual([CALL]);
    }
  });

  it("reports no place for an index tmux did not print as a number", async () => {
    for (const changes of [
      { windowIndex: "" },
      { windowIndex: "#{window_index}" },
      { paneIndex: "x" },
      { paneId: "" },
    ]) {
      const { exec, calls } = fakeExec({ [CALL]: answer(changes) });
      expect(await readTmuxPlace(ENV, exec)).toEqual({ kind: "refused" });
      expect(calls).toEqual([CALL]);
    }
  });

  it("drops a layout past 2000 characters and keeps the other nine members", async () => {
    const layout = "b2f1,".repeat(401);
    expect(layout.length).toBeGreaterThan(2_000);
    const { exec, calls } = fakeExec({ [CALL]: answer({ layout }) });
    const reading = await readTmuxPlace(ENV, exec);
    const { layout: _dropped, ...withoutLayout } = PLACE;
    expect(reading).toEqual({ kind: "place", place: withoutLayout });
    // Absent, not undefined: the key must not reach the wire at all.
    expect(reading.kind === "place" && Object.hasOwn(reading.place, "layout")).toBe(false);
    expect(calls).toEqual([CALL]);
  });

  it("says a call timed out rather than refused, so the caller keeps its last place", async () => {
    jest.useFakeTimers();
    const answered = fakeExec({ [CALL]: answer() });
    const first = await readTmuxPlace(ENV, answered.exec);
    expect(first).toEqual({ kind: "place", place: PLACE });

    const { exec, seen } = slowTmux(null);
    let settled = false;
    const pending = readTmuxPlace(ENV, exec).finally(() => {
      settled = true;
    });
    jest.advanceTimersByTime(PANE_BOUND_MS - 1);
    await settle();
    expect({ settled, aborted: seen.signal?.aborted }).toEqual({ settled: false, aborted: false });
    jest.advanceTimersByTime(1);
    await settle();
    expect({ settled, aborted: seen.signal?.aborted }).toEqual({ settled: true, aborted: true });
    const second = await pending;
    // Nothing to report: a caller holding `first` keeps it, rather than clearing the session's
    // place because one loaded tmux server was slow.
    expect(second).toEqual({ kind: "timedOut" });
    expect(seen.calls).toEqual([CALL]);
    expect(answered.calls).toEqual([CALL]);
  });

  it("reads the place on the call after one that never answered", async () => {
    jest.useFakeTimers();
    const calls: string[] = [];
    let answers = false;
    const exec: ExecFn = (file, args) => {
      calls.push([file, ...args].join(" "));
      const pending = Promise.withResolvers<{ stdout: string }>();
      if (answers) pending.resolve({ stdout: answer() });
      return pending.promise;
    };

    let settled = false;
    const pending = readTmuxPlace(ENV, exec).finally(() => {
      settled = true;
    });
    jest.advanceTimersByTime(PANE_BOUND_MS);
    await settle();
    expect(settled).toBe(true);
    expect(await pending).toEqual({ kind: "timedOut" });

    answers = true;
    expect(await readTmuxPlace(ENV, exec)).toEqual({ kind: "place", place: PLACE });
    expect(calls).toEqual([CALL, CALL]);
  });

  it("ignores an answer that lands after the bound", async () => {
    jest.useFakeTimers();
    const { exec, seen } = slowTmux(PANE_BOUND_MS + 500);
    let settled = false;
    const pending = readTmuxPlace(ENV, exec).finally(() => {
      settled = true;
    });
    jest.advanceTimersByTime(PANE_BOUND_MS + 500);
    await settle();
    expect({ settled, aborted: seen.signal?.aborted }).toEqual({ settled: true, aborted: true });
    expect(await pending).toEqual({ kind: "timedOut" });
    expect(seen.calls).toEqual([CALL]);
  });
});

import { afterAll, beforeAll, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { SQL } from "bun";

/**
 * message-wait.ts measured over a session store of this test's own: a database it creates on the
 * server `LEGION_TEST_PG_DSN` names, holding the two tables
 * `packages/daemon/internal/ompsessions/ompsessions.go` defines and nothing else. The script's
 * `paths()` is unscoped, so the measurement sees exactly the fixture below; the shared session
 * store `OMP_SESSION_SQL_DSN_FILE` names holds every agent's conversation and is no route for a
 * test. The surface is the command a reader runs, spawned, read by its stdout, stderr and exit
 * code: nothing of the script is imported.
 */

const SCRIPT = join(import.meta.dir, "..", "message-wait.ts");

const SERVER = process.env.LEGION_TEST_PG_DSN;
if (SERVER === undefined || SERVER === "") {
  throw new Error(
    "LEGION_TEST_PG_DSN must name the PostgreSQL server this test creates its own session-store " +
      "database on, and drops again. This test never skips: without that server it has measured " +
      "nothing. It is the pod's and CI's own test server, never the shared Oh My Pi session store " +
      "the file OMP_SESSION_SQL_DSN_FILE names."
  );
}

const MINUTE = 60_000;
/** The threshold the run uses, which is the script's default: every printed wait passes it. */
const THRESHOLD = "10m00s";
/** The window the run asks for, as the scope line spells it. */
const WINDOW = "24h00m00s";

/**
 * Every session is launched three hours before the run, computed from the clock at run time, so
 * the 24h window always holds it and the fixture never goes stale.
 */
const LAUNCHED_AT = Date.now() - 3 * 60 * MINUTE;
const LAST_WRITE_MS = LAUNCHED_AT + 60 * MINUTE;

/** An instant `offset` after the launch, in the shape every record of the store carries. */
const at = (offset: number): string => new Date(LAUNCHED_AT + offset).toISOString();

/** One record of a session's JSONL. */
const line = (offset: number, fields: Record<string, unknown>): string =>
  JSON.stringify({ ...fields, timestamp: at(offset) });

/** A card the host persisted: the arrival the measurement reads, with the plugin's `details`. */
const card = (offset: number, details: Record<string, unknown>): string =>
  line(offset, {
    type: "custom_message",
    customType: "envoy-message",
    content: "envoy:\n  from: another session",
    display: true,
    details,
  });
const assistant = (offset: number): string =>
  line(offset, { type: "message", message: { role: "assistant", content: "answered" } });

/** A made-up session: this repository is public, so no real id, role or deployment is named. */
interface Fixture {
  readonly id: string;
  readonly role: string;
  /** The records after the role claim every session opens with. */
  readonly lines: readonly string[];
  /** When set, the last line is stored as a `omp_session_files_parts` row, as a long session is. */
  readonly tailInPart?: true;
}

/** The arrival of every session below, two minutes after its launch. */
const ARRIVED = 2 * MINUTE;
const ARRIVAL = at(ARRIVED);

const HELD: Fixture = {
  id: "01a10000-0000-7000-8000-000000000001",
  role: "example-implementer",
  lines: [
    // The call opens before the arrival and closes after it, so it was open at the arrival.
    line(1 * MINUTE, {
      type: "custom",
      customType: "tool_execution_start",
      data: { toolName: "bash", toolCallId: "call-held" },
    }),
    card(ARRIVED, { envoyCard: "11110000-0000-7000-8000-000000000001", handOver: "run-active" }),
    line(20 * MINUTE, {
      type: "message",
      message: { role: "toolResult", toolCallId: "call-held" },
    }),
    assistant(25 * MINUTE),
  ],
  tailInPart: true,
};

const UNHELD: Fixture = {
  id: "01a10000-0000-7000-8000-000000000002",
  role: "example-planner",
  lines: [
    card(ARRIVED, { envoyCard: "11110000-0000-7000-8000-000000000002", handOver: "idle" }),
    assistant(14 * MINUTE),
  ],
};

const UNANSWERED: Fixture = {
  id: "01a10000-0000-7000-8000-000000000003",
  role: "example-tester",
  lines: [
    card(ARRIVED, { envoyCard: "11110000-0000-7000-8000-000000000003", handOver: "prompt-setup" }),
    // No assistant message follows, so the wait is no measured wait; this is the last record.
    line(40 * MINUTE, { type: "message", message: { role: "user", content: "a turn of its own" } }),
  ],
};

const STAMPED: Fixture = {
  id: "01a10000-0000-7000-8000-000000000004",
  role: "example-reviewer",
  lines: [
    card(ARRIVED, { envoyCard: "11110000-0000-7000-8000-000000000004", handOver: "startup" }),
    assistant(17 * MINUTE),
  ],
};

const UNSTAMPED: Fixture = {
  id: "01a10000-0000-7000-8000-000000000005",
  role: "example-merger",
  // A card the plugin wrote before it stamped the host state: a replay frame and no `handOver`.
  lines: [card(ARRIVED, { event_id: "evt-example" }), assistant(16 * MINUTE)],
};

const INERT: Fixture = {
  id: "01a10000-0000-7000-8000-000000000006",
  role: "example-architect",
  // The inert path stamps `inert` and carries no card id, since the wake tracks no inert card.
  lines: [card(ARRIVED, { handOver: "inert" }), assistant(15 * MINUTE)],
};

const LOCAL_NOTICE: Fixture = {
  id: "01a10000-0000-7000-8000-000000000007",
  role: "example-watcher",
  lines: [
    // The plugin's own notice: no Envoy message another session sent. Nothing answers it and the
    // session runs on for another 43 minutes, so a counted arrival here would be on a line.
    card(ARRIVED, { localNotice: true }),
    line(45 * MINUTE, { type: "message", message: { role: "user", content: "a turn of its own" } }),
  ],
};

const FIXTURES: readonly Fixture[] = [
  HELD,
  UNHELD,
  UNANSWERED,
  STAMPED,
  UNSTAMPED,
  INERT,
  LOCAL_NOTICE,
];

/** The launch instant as Oh My Pi names a session file with it. */
const STAMP = new Date(LAUNCHED_AT).toISOString().replaceAll(":", "-").replace(".", "-");
/** Where a session file sits: a made-up directory, since this repository is public. */
const DIRECTORY = "/example/sessions";
/** What the report calls the session: the role it claimed and its own id. */
const labelOf = (session: Fixture): string => `${session.role} ${session.id}`;

const scratch = await mkdtemp(join(tmpdir(), "message-wait-"));
const DATABASE = `legion_message_wait_${randomUUID().replaceAll("-", "")}`;
const admin = new SQL(SERVER);
const storeUrl = new URL(SERVER);
storeUrl.pathname = `/${DATABASE}`;
const store = new SQL(storeUrl.toString());
// The fixture is built in the `beforeAll` below, which Bun follows with this hook even when it
// throws, so a create or an insert that fails still takes the database and the scratch directory
// with it.
afterAll(async () => {
  await rm(scratch, { recursive: true, force: true });
  await store.close();
  await admin.unsafe(`drop database if exists ${DATABASE} with (force)`);
  await admin.close();
});

/** What the spawned run printed: its exit code, its stderr, and its stdout line by line. */
let code = -1;
let stderr = "";
let printed: string[] = [];

// Creating the database, writing the fixture and spawning the script take longer than the five
// seconds a hook is given by default, whenever the server answers slowly.
beforeAll(async () => {
  await admin.unsafe(`create database ${DATABASE} strategy file_copy`);
  await store.unsafe(
    "create table omp_session_files (path TEXT PRIMARY KEY, content TEXT NOT NULL, " +
      "mtime_ms BIGINT NOT NULL, title TEXT, title_source TEXT, title_updated_at TEXT, " +
      "byte_len BIGINT)"
  );
  await store.unsafe(
    "create table omp_session_files_parts (path TEXT NOT NULL, start_offset BIGINT NOT NULL, " +
      "content TEXT NOT NULL, PRIMARY KEY (path, start_offset))"
  );
  for (const session of FIXTURES) {
    // Every session opens with the claim of the role a message was delivered to.
    const claim = { type: "custom", customType: "envoy-role-claim", data: { role: session.role } };
    const records = [line(0, claim), ...session.lines];
    const text = `${records.join("\n")}\n`;
    const head = session.tailInPart === true ? `${records.slice(0, -1).join("\n")}\n` : text;
    const tail = text.slice(head.length);
    const path = `${DIRECTORY}/${STAMP}_${session.id}.jsonl`;
    await store`insert into omp_session_files (path, content, mtime_ms, byte_len)
      values (${path}, ${head}, ${LAST_WRITE_MS}, ${Buffer.byteLength(text)})`;
    if (tail !== "") {
      await store`insert into omp_session_files_parts (path, start_offset, content)
        values (${path}, ${Buffer.byteLength(head)}, ${tail})`;
    }
  }

  const dsnFile = join(scratch, "session-store.url");
  await writeFile(dsnFile, `${storeUrl.toString()}\n`);

  const child = Bun.spawn(["bun", SCRIPT, "--since", "24h", "--dsn-file", dsnFile], {
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, failures, exited] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  printed = stdout.split("\n");
  stderr = failures;
  code = exited;
}, 60_000);

/** One printed wait line: the wait, the arrival, what held it, the host state and the session. */
const waitLine = (wait: string, held: string, state: string, session: Fixture): string =>
  `  ${wait}  ${ARRIVAL}  ${held}  hand-over: ${state}  ${labelOf(session)}`;
/** What a line says of a wait no call ran in. */
const NO_CALL = "open: none  no call ran during the wait";

// The fixture's own numbers. Seven sessions; six carry a card the measurement counts, since the
// seventh carries only the plugin's own notice. Five of those six were answered and each waited
// longer than the threshold, one of them with a bash call open; the sixth was never answered.
const SESSIONS = FIXTURES.length;
const WITH_ARRIVALS = SESSIONS - 1;
const MEASURED = 5;
const HELD_OVER = 1;
const UNHELD_OVER = MEASURED - HELD_OVER;
const NOT_ANSWERED = 1;

const EXPECTED: string[] = [
  // The plugin's own notice is in no count: the session that carries it is one of the seven
  // launched, and the sessions that took a message are the other six.
  `${SESSIONS} sessions launched within ${WINDOW} of now; ${WITH_ARRIVALS} of them took an ` +
    "Envoy message",
  "each line below: the wait, the arrival, the calls open at the arrival (the calls the " +
    "verdict turns on), the call that took the longest part of the wait, the host state when " +
    "the plugin handed the card over, and the session; the plugin's own notices and its own " +
    "wake are in no count and on no line, and a card written before the plugin stamped the " +
    "state prints unstamped",
  "",
  // The group the verdict turns on, and the one wait in it: the bash call opened a minute before
  // the arrival and closed eighteen minutes after it, so it was open when the message arrived.
  `waits of ${THRESHOLD} or more with a bash or task call open (the verdict fails on these): ` +
    `${HELD_OVER}`,
  waitLine("23m00s", "open: bash  longest call in the wait bash 18m00s", "run-active", HELD),
  "",
  `waits of ${THRESHOLD} or more with no bash or task call open: ${UNHELD_OVER}`,
  // Every reported wait carries the host state at the hand-over: the stamped card prints the
  // state it carries, the card whose `details` hold a replay frame and no state prints
  // `unstamped`, and the inert card prints `inert`.
  waitLine("15m00s", NO_CALL, "startup", STAMPED),
  waitLine("14m00s", NO_CALL, "unstamped", UNSTAMPED),
  waitLine("13m00s", NO_CALL, "inert", INERT),
  waitLine("12m00s", NO_CALL, "idle", UNHELD),
  "",
  // One unanswered arrival, not two: the notice is in this count no more than in the one above,
  // though nothing answered it either.
  `arrivals no assistant message answered (no measured wait): ${NOT_ANSWERED}`,
  // The unanswered arrival carries the state at the hand-over as well, and the notice is on no
  // line: a line for it would read 43m00s, the time its session ran on after it.
  `  38m00s to the session's last record  ${ARRIVAL}  ${NO_CALL}  hand-over: prompt-setup  ` +
    labelOf(UNANSWERED),
  "",
  // The verdict counts the five arrivals an assistant message answered, the notice left out, and
  // fails on the one held wait above.
  `verdict: ${MEASURED} arrivals measured, ${MEASURED} waited ${THRESHOLD} or more, ` +
    `${HELD_OVER} of those with a bash or task call open. ` +
    "The measurement passes only while that last count is 0.",
  "",
];

test("the run reports the fixture's waits, line by line", () => {
  expect({ code, stderr }).toEqual({ code: 0, stderr: "" });
  expect(printed).toEqual(EXPECTED);
});

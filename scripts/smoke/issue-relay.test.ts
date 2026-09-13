import { describe, expect, test } from "bun:test";
import issueCreatedChild from "../../packages/daemon/src/daemon/__tests__/fixtures/dispatch/issue-created-child.json";
import issueCreatedRoot from "../../packages/daemon/src/daemon/__tests__/fixtures/dispatch/issue-created-root.json";
import issueUpdatedHuman from "../../packages/daemon/src/daemon/__tests__/fixtures/dispatch/legsmoke-3-issue.updated-human-todo.json";
import { dispatchIssueEvent } from "../../packages/daemon/src/daemon/dispatch-events";
import { envelopeValidation } from "./envoy-bridge";
import {
  type DispatchEventRow,
  EVENT_PAGE_LIMIT,
  envelopeForEvent,
  messageId,
  POLL_INTERVAL_MS,
  type RelayConfig,
  relayConfigFromEnvironment,
  runRelay,
} from "./issue-relay";

const environment = {
  SMOKE_ROOT_ISSUE: "LEGSMOKE-7",
  SMOKE_RIG_NATS: "nats://127.0.0.1:14222",
  DISPATCH_URL: "http://dispatch.test/",
  DISPATCH_TOKEN: "test-dispatch-token",
};

describe("relayConfigFromEnvironment", () => {
  test("derives the Dispatch project from the root key and strips the URL's trailing slash", () => {
    expect(relayConfigFromEnvironment(environment)).toEqual({
      rootIssue: "LEGSMOKE-7",
      project: "LEGSMOKE",
      dispatchUrl: "http://dispatch.test",
      token: "test-dispatch-token",
      downstreamUrl: "nats://127.0.0.1:14222",
    });
  });

  test.each([
    "LEGSMOKE-*",
    "LEGSMOKE-1.>",
    "legsmoke-1",
    "",
  ])("rejects a root key that is not one literal Dispatch issue key: %j", (rootIssue) => {
    expect(() =>
      relayConfigFromEnvironment({ ...environment, SMOKE_ROOT_ISSUE: rootIssue })
    ).toThrow("SMOKE_ROOT_ISSUE must be a literal Dispatch issue key");
  });

  test.each([
    "SMOKE_RIG_NATS",
    "DISPATCH_URL",
    "DISPATCH_TOKEN",
  ] as const)("names %s when it is empty", (variable) => {
    expect(() => relayConfigFromEnvironment({ ...environment, [variable]: " " })).toThrow(
      `${variable} is required`
    );
  });
});

describe("envelopeForEvent", () => {
  const fixtures: Array<[string, DispatchEventRow]> = [
    ["issue-created-root", issueCreatedRoot as DispatchEventRow],
    ["issue-created-child", issueCreatedChild as DispatchEventRow],
    ["legsmoke-3-issue.updated-human-todo", issueUpdatedHuman as DispatchEventRow],
  ];

  test.each(
    fixtures
  )("%s: the envelope is valid for the daemon and decodes to the fixture's identity", (_name, row) => {
    const envelope = envelopeForEvent(row);

    expect(envelopeValidation(JSON.stringify(envelope))).toEqual({
      valid: true,
      shape: "current",
    });
    const decoded = dispatchIssueEvent(envelope);
    expect(decoded).toMatchObject({
      key: row.issue_key,
      seq: row.seq,
      type: row.type,
      notify: row.notify,
      eventId: `dispatch-${row.id}`,
    });
    expect(decoded.payload).toEqual(row.payload);
    expect(envelope.event_id).toBe(`dispatch-${row.id}`);
    expect(envelope.dedupe_key).toBe(envelope.event_id);
    expect(envelope.trace_id).toBe(envelope.event_id);
    expect(envelope.source).toBe("dispatch");
    expect(envelope.source_event_id).toBe(String(row.id));
    expect(envelope.topic).toBe(`notifications.dispatch.issue.${row.issue_key}.${row.type}`);
    expect(envelope.issued_at).toBe(Date.parse(row.created_at));
    expect(envelope.payload_summary).toBe(`${row.issue_key} ${row.type.replace(".", " ")}`);
    expect(messageId(envelope)).toBe(
      `dispatch-${row.id}:notifications.dispatch.issue.${row.issue_key}.${row.type}`
    );
  });

  test("carries source_session only for a session actor", () => {
    expect(envelopeForEvent(issueCreatedRoot as DispatchEventRow).source_session).toBe(
      "legion-daemon:LEGSMOKE"
    );
    expect(envelopeForEvent(issueUpdatedHuman as DispatchEventRow).source_session).toBeUndefined();
  });

  test("summarises an ask the way the production outbox does, whitespace collapsed", () => {
    const envelope = envelopeForEvent({
      ...(issueUpdatedHuman as DispatchEventRow),
      type: "ask.opened",
      payload: { id: "ask-1", question: "Approve   the\n design?", urgency: "med" },
    });
    expect(envelope.payload_summary).toBe("LEGSMOKE-3 ask opened: Approve the design?");
    expect(envelope.urgency).toBe("med");
  });
});

interface FakeIssue {
  key: string;
  parent: string | null;
}

interface FakeDispatch {
  issues: FakeIssue[];
  events: DispatchEventRow[];
  requests: string[];
  failure: ((url: string) => Response | Error | undefined) | undefined;
  fetch: (url: string, init?: RequestInit) => Promise<Response>;
}

/** Event ids double as milliseconds past a fixed instant, so every row has its own strictly
 * increasing `created_at` and an issue's `updated_at` (the greatest of its rows') is exact. */
const EPOCH_MS = Date.parse("2026-09-13T10:00:00.000Z");
const createdAt = (id: number): string => new Date(EPOCH_MS + id).toISOString();
/** An issue that has no rows yet was created before any row landed. */
const NO_ROWS_UPDATED_AT = new Date(EPOCH_MS - 1000).toISOString();

const row = (id: number, key: string, seq: number, type = "issue.updated"): DispatchEventRow => ({
  id,
  issue_key: key,
  artifact_id: null,
  project: "LEGSMOKE",
  seq,
  type,
  actor: { kind: "session", id: "legion-daemon:LEGSMOKE" },
  notify: false,
  created_at: createdAt(id),
  payload: { key, parent: null, status: "todo" },
});

const listUrl = "http://dispatch.test/api/v1/issues?project=LEGSMOKE";
const listSinceUrl = (updatedAt: string) =>
  `${listUrl}&updated_since=${encodeURIComponent(updatedAt)}`;
const eventsUrl = (key: string, after: number) =>
  `http://dispatch.test/api/v1/issues/${key}/events?after=${after}&limit=${EVENT_PAGE_LIMIT}`;

/** Serves `GET /api/v1/issues?project=&updated_since=` from `issues` + `events` the way Dispatch
 * does (every issue's summary carries `parent`, `last_seq`, and an `updated_at` raised by each of
 * its rows; `updated_since` is inclusive) and `GET /api/v1/issues/<key>/events` from `events`. */
function fakeDispatch(issues: FakeIssue[], events: DispatchEventRow[]): FakeDispatch {
  const summary = (issue: FakeIssue) => {
    const rows = dispatch.events.filter((event) => event.issue_key === issue.key);
    return {
      key: issue.key,
      title: `Fixture ${issue.key}`,
      status: "todo",
      priority: null,
      rank: "a0",
      labels: [],
      parent: issue.parent,
      updated_at:
        rows.length === 0 ? NO_ROWS_UPDATED_AT : createdAt(Math.max(...rows.map((r) => r.id))),
      last_seq: rows.length === 0 ? 0 : Math.max(...rows.map((r) => r.seq)),
      open_asks: 0,
    };
  };
  const dispatch: FakeDispatch = {
    issues,
    events,
    requests: [],
    failure: undefined,
    fetch: async (url: string, init?: RequestInit) => {
      dispatch.requests.push(url);
      const failure = dispatch.failure?.(url);
      if (failure instanceof Error) throw failure;
      if (failure) return failure;
      const headers = new Headers(init?.headers);
      if (headers.get("authorization") !== "Bearer test-dispatch-token") {
        return Response.json({ code: "UNAUTHORIZED" }, { status: 401 });
      }
      const parsed = new URL(url);
      const eventsMatch = parsed.pathname.match(/^\/api\/v1\/issues\/([^/]+)\/events$/);
      if (eventsMatch) {
        const after = Number(parsed.searchParams.get("after") ?? "0");
        const limit = Number(parsed.searchParams.get("limit"));
        if (!(limit >= 1 && limit <= 200)) {
          return Response.json({ code: "INVALID_QUERY" }, { status: 400 });
        }
        return Response.json(
          dispatch.events
            .filter((event) => event.issue_key === eventsMatch[1] && event.seq > after)
            .sort((left, right) => left.seq - right.seq)
            .slice(0, limit)
        );
      }
      if (parsed.pathname === "/api/v1/issues") {
        if (parsed.searchParams.get("project") !== "LEGSMOKE") {
          return Response.json({ code: "UNEXPECTED_PROJECT" }, { status: 400 });
        }
        const since = parsed.searchParams.get("updated_since");
        if (since !== null && !Number.isFinite(Date.parse(since))) {
          return Response.json({ code: "INVALID_UPDATED_SINCE" }, { status: 400 });
        }
        return Response.json(
          dispatch.issues
            .map(summary)
            .filter((entry) => since === null || Date.parse(entry.updated_at) >= Date.parse(since))
        );
      }
      return Response.json({ code: "NOT_FOUND" }, { status: 404 });
    },
  };
  return dispatch;
}

interface Published {
  subject: string;
  msgID: string;
  envelope: Record<string, unknown>;
}

interface Harness {
  dispatch: FakeDispatch;
  published: Published[];
  log: string[];
  backoffs: number[];
  connections: number;
  drained: boolean;
  /** Requests made since the previous call (each tick's own set). */
  requestsSince: () => string[];
  run: (ticks: number, onTick?: (tick: number) => void) => Promise<void>;
}

const config: RelayConfig = {
  rootIssue: "LEGSMOKE-1",
  project: "LEGSMOKE",
  dispatchUrl: "http://dispatch.test",
  token: "test-dispatch-token",
  downstreamUrl: "nats://127.0.0.1:14222",
};

function harness(issues: FakeIssue[], events: DispatchEventRow[]): Harness {
  const dispatch = fakeDispatch(issues, events);
  const published: Published[] = [];
  const seen = new Set<string>();
  const log: string[] = [];
  const backoffs: number[] = [];
  let requestMark = 0;
  const state: Harness = {
    dispatch,
    published,
    log,
    backoffs,
    connections: 0,
    drained: false,
    requestsSince: () => {
      const slice = dispatch.requests.slice(requestMark);
      requestMark = dispatch.requests.length;
      return slice;
    },
    run: async (ticks: number, onTick?: (tick: number) => void) => {
      const controller = new AbortController();
      let polls = 0;
      await runRelay(config, {
        fetch: dispatch.fetch,
        connect: async () => {
          state.connections += 1;
          return {
            jetstream: () => ({
              publish: async (subject: string, data: Uint8Array, options?: { msgID?: string }) => {
                const msgID = options?.msgID ?? "";
                const duplicate = seen.has(msgID);
                seen.add(msgID);
                published.push({
                  subject,
                  msgID,
                  envelope: JSON.parse(new TextDecoder().decode(data)),
                });
                return { seq: published.length, duplicate };
              },
            }),
            drain: async () => {
              state.drained = true;
            },
          };
        },
        // A backoff sleep always follows its RELAY RETRY line; every other sleep is the poll
        // interval (whose 2000 ms coincides with the second backoff step). `onTick` runs between
        // ticks, after a successful one, so a test can land new rows "between polls".
        sleep: async (ms: number) => {
          if (log.at(-1)?.startsWith("RELAY RETRY")) {
            backoffs.push(ms);
            return;
          }
          expect(ms).toBe(POLL_INTERVAL_MS);
          polls += 1;
          if (polls >= ticks) controller.abort();
          else onTick?.(polls);
        },
        log: (line: string) => {
          log.push(line);
        },
        signal: controller.signal,
      });
    },
  };
  return state;
}

const relayed = (rig: Harness) => rig.log.filter((line) => line.startsWith("RELAYED"));
const tracking = (rig: Harness) => rig.log.filter((line) => line.startsWith("RELAY TRACKING"));
const retries = (rig: Harness) => rig.log.filter((line) => line.startsWith("RELAY RETRY"));
const ready = (rig: Harness) => rig.log.filter((line) => line.startsWith("RELAY READY"));

describe("runRelay", () => {
  test("one project list per tick: discovers the root's transitive children from it, fetches events only for tracked keys whose last_seq grew, publishes in global id order, and asks only for summaries updated since the watermark afterwards", async () => {
    // The grandchild is listed before its parent, so the closure needs a second pass; another
    // rig's root and that root's child share the project and must never be fetched or published.
    const rig = harness(
      [
        { key: "LEGSMOKE-1", parent: null },
        { key: "LEGSMOKE-3", parent: "LEGSMOKE-2" },
        { key: "LEGSMOKE-2", parent: "LEGSMOKE-1" },
        { key: "LEGSMOKE-99", parent: null },
        { key: "LEGSMOKE-98", parent: "LEGSMOKE-99" },
      ],
      [
        row(100, "LEGSMOKE-1", 1, "issue.created"),
        row(101, "LEGSMOKE-2", 1, "issue.created"),
        row(102, "LEGSMOKE-1", 2, "child.status"),
        row(103, "LEGSMOKE-3", 1, "issue.created"),
        row(104, "LEGSMOKE-99", 1, "issue.created"),
        row(105, "LEGSMOKE-2", 2),
        row(106, "LEGSMOKE-98", 1, "issue.created"),
      ]
    );
    const firstTick: string[] = [];
    await rig.run(2, () => firstTick.push(...rig.requestsSince()));

    expect(firstTick[0]).toBe(listUrl);
    expect(firstTick.slice(1).sort()).toEqual(
      [eventsUrl("LEGSMOKE-1", 0), eventsUrl("LEGSMOKE-2", 0), eventsUrl("LEGSMOKE-3", 0)].sort()
    );
    expect(tracking(rig)).toEqual([
      "RELAY TRACKING LEGSMOKE-2 parent=LEGSMOKE-1",
      "RELAY TRACKING LEGSMOKE-3 parent=LEGSMOKE-2",
    ]);
    expect(rig.published.map((entry) => entry.envelope.event_id)).toEqual([
      "dispatch-100",
      "dispatch-101",
      "dispatch-102",
      "dispatch-103",
      "dispatch-105",
    ]);
    expect(rig.published.map((entry) => entry.subject)).toEqual([
      "notifications.dispatch.issue.LEGSMOKE-1.issue.created",
      "notifications.dispatch.issue.LEGSMOKE-2.issue.created",
      "notifications.dispatch.issue.LEGSMOKE-1.child.status",
      "notifications.dispatch.issue.LEGSMOKE-3.issue.created",
      "notifications.dispatch.issue.LEGSMOKE-2.issue.updated",
    ]);
    expect(rig.published.map((entry) => entry.msgID)).toEqual(
      rig.published.map((entry) => `${entry.envelope.dedupe_key}:${entry.subject}`)
    );
    expect(relayed(rig)).toEqual([
      "RELAYED subject=notifications.dispatch.issue.LEGSMOKE-1.issue.created event=dispatch-100 seq=1 stream_seq=1 duplicate=false",
      "RELAYED subject=notifications.dispatch.issue.LEGSMOKE-2.issue.created event=dispatch-101 seq=1 stream_seq=2 duplicate=false",
      "RELAYED subject=notifications.dispatch.issue.LEGSMOKE-1.child.status event=dispatch-102 seq=2 stream_seq=3 duplicate=false",
      "RELAYED subject=notifications.dispatch.issue.LEGSMOKE-3.issue.created event=dispatch-103 seq=1 stream_seq=4 duplicate=false",
      "RELAYED subject=notifications.dispatch.issue.LEGSMOKE-2.issue.updated event=dispatch-105 seq=2 stream_seq=5 duplicate=false",
    ]);
    expect(ready(rig)).toEqual([
      "RELAY READY root=LEGSMOKE-1 project=LEGSMOKE dispatch=http://dispatch.test downstream=nats://127.0.0.1:14222",
    ]);
    // The second tick, with nothing new: exactly one request, asking from the watermark (the
    // greatest updated_at seen: LEGSMOKE-98's row 106, another rig's issue this relay only ever
    // reads a summary of), and no /events at all since no tracked key's last_seq moved.
    expect(rig.requestsSince()).toEqual([listSinceUrl(createdAt(106))]);
    // Nothing outside the root's tree was ever fetched from /events or published.
    expect(rig.dispatch.requests.filter((url) => /LEGSMOKE-9[89]\/events/.test(url))).toEqual([]);
    expect(rig.published.some((entry) => entry.subject.includes("LEGSMOKE-9"))).toBe(false);
    expect(rig.connections).toBe(1);
    expect(rig.drained).toBe(true);
  });

  test("rows and children that land between ticks are picked up from the updated summaries alone: events are fetched from the acked cursor for the changed key only, a new child is tracked and fetched, an unchanged key and another rig's changed issue are left alone", async () => {
    const rig = harness(
      [
        { key: "LEGSMOKE-1", parent: null },
        { key: "LEGSMOKE-2", parent: "LEGSMOKE-1" },
        { key: "LEGSMOKE-99", parent: null },
      ],
      [row(100, "LEGSMOKE-1", 1, "issue.created"), row(101, "LEGSMOKE-2", 1, "issue.created")]
    );
    const requestsByTick: string[][] = [];
    await rig.run(3, (tick) => {
      requestsByTick.push(rig.requestsSince());
      if (tick === 1) {
        rig.dispatch.events.push(
          row(110, "LEGSMOKE-2", 2),
          row(111, "LEGSMOKE-99", 1, "issue.created"),
          row(112, "LEGSMOKE-4", 1, "issue.created")
        );
        rig.dispatch.issues.push({ key: "LEGSMOKE-4", parent: "LEGSMOKE-1" });
      }
    });
    requestsByTick.push(rig.requestsSince());

    expect(requestsByTick[1][0]).toBe(listSinceUrl(createdAt(101)));
    expect(requestsByTick[1].slice(1).sort()).toEqual(
      [eventsUrl("LEGSMOKE-2", 1), eventsUrl("LEGSMOKE-4", 0)].sort()
    );
    expect(tracking(rig)).toEqual([
      "RELAY TRACKING LEGSMOKE-2 parent=LEGSMOKE-1",
      "RELAY TRACKING LEGSMOKE-4 parent=LEGSMOKE-1",
    ]);
    expect(rig.published.map((entry) => entry.envelope.event_id)).toEqual([
      "dispatch-100",
      "dispatch-101",
      "dispatch-110",
      "dispatch-112",
    ]);
    // Third tick: the watermark moved to the newest summary seen (LEGSMOKE-4's row 112), nothing
    // changed, one request.
    expect(requestsByTick[2]).toEqual([listSinceUrl(createdAt(112))]);
    expect(rig.dispatch.requests.filter((url) => url.includes("LEGSMOKE-1/events"))).toEqual([
      eventsUrl("LEGSMOKE-1", 0),
    ]);
    expect(rig.dispatch.requests.some((url) => url.includes("LEGSMOKE-99/events"))).toBe(false);
  });

  test("pages a key whose backlog exceeds one page", async () => {
    const backlog = Array.from({ length: EVENT_PAGE_LIMIT + 3 }, (_, index) =>
      row(1000 + index, "LEGSMOKE-1", index + 1)
    );
    const rig = harness([{ key: "LEGSMOKE-1", parent: null }], backlog);

    await rig.run(1);

    expect(rig.published).toHaveLength(EVENT_PAGE_LIMIT + 3);
    expect(rig.dispatch.requests).toEqual([
      listUrl,
      eventsUrl("LEGSMOKE-1", 0),
      eventsUrl("LEGSMOKE-1", EVENT_PAGE_LIMIT),
    ]);
  });

  test.each([
    ["key", { key: "legsmoke-1", parent: null }],
    ["parent", { key: "LEGSMOKE-5", parent: "not a key" }],
  ])("a summary whose %s is not a Dispatch issue key is a loud shape error, never a silent drop", async (field, bogus) => {
    const rig = harness([{ key: "LEGSMOKE-1", parent: null }], [row(100, "LEGSMOKE-1", 1)]);
    let served = false;
    rig.dispatch.failure = (url) => {
      if (served || !url.startsWith(listUrl)) return undefined;
      served = true;
      return Response.json([
        { ...bogus, last_seq: 1, updated_at: createdAt(100) },
        {
          key: "LEGSMOKE-1",
          parent: null,
          last_seq: 1,
          updated_at: createdAt(100),
        },
      ]);
    };

    await rig.run(1);

    expect(retries(rig)).toHaveLength(1);
    expect(retries(rig)[0]).toMatch(
      new RegExp(
        `^RELAY RETRY attempt=1 next=1000ms: GET /api/v1/issues\\?project=LEGSMOKE returned an unexpected shape: 0\\.${field}: `
      )
    );
    expect(retries(rig)[0]).not.toContain("\n");
    // Nothing from the rejected answer was used; the next, well-formed answer is.
    expect(rig.published.map((entry) => entry.envelope.event_id)).toEqual(["dispatch-100"]);
    expect(rig.log.filter((line) => line.startsWith("RELAY RECOVERED"))).toEqual([
      "RELAY RECOVERED after 1 attempts",
    ]);
  });

  test("retries a Dispatch failure with doubling backoff, keeps what earlier answers taught it, advances no cursor, then publishes the missed row and recovers", async () => {
    const rig = harness([{ key: "LEGSMOKE-1", parent: null }], [row(100, "LEGSMOKE-1", 1)]);
    let failures = 0;
    rig.dispatch.failure = (url) => {
      // After the first tick: the /events read fails seven times (network, 502, then non-JSON
      // bodies). Meanwhile the list answers nothing at all, so the retried fetch can only come
      // from the summary the relay already merged before the first failure.
      if (rig.published.length === 0) return undefined;
      if (url.startsWith(listUrl)) return failures >= 1 ? Response.json([]) : undefined;
      if (!url.includes("/events?") || failures >= 7) return undefined;
      failures += 1;
      if (failures === 1) return new Error("connect ECONNREFUSED");
      if (failures === 2) return new Response("<html>bad gateway</html>", { status: 502 });
      return new Response("not json", { status: 200 });
    };
    await rig.run(3, (tick) => {
      if (tick === 1) rig.dispatch.events.push(row(101, "LEGSMOKE-1", 2));
    });

    expect(rig.backoffs).toEqual([1000, 2000, 4000, 8000, 16000, 30000, 30000]);
    expect(retries(rig)).toHaveLength(7);
    expect(retries(rig)[0]).toMatch(/^RELAY RETRY attempt=1 next=1000ms: .*ECONNREFUSED/);
    expect(retries(rig)[1]).toMatch(/^RELAY RETRY attempt=2 next=2000ms: .*502/);
    expect(retries(rig)[6]).toMatch(/^RELAY RETRY attempt=7 next=30000ms: /);
    expect(rig.log.filter((line) => line.startsWith("RELAY RECOVERED"))).toEqual([
      "RELAY RECOVERED after 7 attempts",
    ]);
    // Every retried read re-asked from the acked cursor (seq 1), eight times: seven failures
    // and the one that succeeded.
    expect(rig.dispatch.requests.filter((url) => url === eventsUrl("LEGSMOKE-1", 1))).toHaveLength(
      8
    );
    expect(rig.published.map((entry) => entry.envelope.event_id)).toEqual([
      "dispatch-100",
      "dispatch-101",
    ]);
    expect(ready(rig)).toHaveLength(1);
  });

  test("a rejected publish is retried without skipping or repeating a row: the key already acked is not fetched again, the one behind is fetched from its cursor", async () => {
    const rig = harness(
      [
        { key: "LEGSMOKE-1", parent: null },
        { key: "LEGSMOKE-2", parent: "LEGSMOKE-1" },
      ],
      [row(100, "LEGSMOKE-1", 1, "issue.created"), row(101, "LEGSMOKE-2", 1, "issue.created")]
    );
    const controller = new AbortController();
    let publishes = 0;
    let polls = 0;
    const published: string[] = [];
    await runRelay(config, {
      fetch: rig.dispatch.fetch,
      connect: async () => ({
        jetstream: () => ({
          publish: async (subject: string) => {
            publishes += 1;
            if (publishes === 2) throw new Error("nats: timeout");
            published.push(subject);
            return { seq: publishes, duplicate: false };
          },
        }),
        drain: async () => {},
      }),
      sleep: async (ms: number) => {
        if (rig.log.at(-1)?.startsWith("RELAY RETRY")) {
          rig.backoffs.push(ms);
          return;
        }
        expect(ms).toBe(POLL_INTERVAL_MS);
        polls += 1;
        if (polls >= 1) controller.abort();
      },
      log: (line: string) => {
        rig.log.push(line);
      },
      signal: controller.signal,
    });

    expect(rig.backoffs).toEqual([1000]);
    expect(retries(rig)).toEqual([
      "RELAY RETRY attempt=1 next=1000ms: publish of dispatch-101 to notifications.dispatch.issue.LEGSMOKE-2.issue.created on the rig NATS failed: nats: timeout",
    ]);
    expect(published).toEqual([
      "notifications.dispatch.issue.LEGSMOKE-1.issue.created",
      "notifications.dispatch.issue.LEGSMOKE-2.issue.created",
    ]);
    expect(rig.dispatch.requests.filter((url) => url.includes("LEGSMOKE-1/events"))).toEqual([
      eventsUrl("LEGSMOKE-1", 0),
    ]);
    expect(rig.dispatch.requests.filter((url) => url.includes("LEGSMOKE-2/events"))).toEqual([
      eventsUrl("LEGSMOKE-2", 0),
      eventsUrl("LEGSMOKE-2", 0),
    ]);
    // READY waits for the first tick that fully succeeds, publishes included.
    expect(rig.log.indexOf(retries(rig)[0])).toBeLessThan(rig.log.indexOf(ready(rig)[0]));
  });

  test("a root with no rows yet is READY once, publishes nothing, and keeps listing once per tick", async () => {
    const rig = harness([{ key: "LEGSMOKE-1", parent: null }], []);

    await rig.run(3);

    expect(rig.published).toEqual([]);
    expect(ready(rig)).toHaveLength(1);
    expect(rig.dispatch.requests).toEqual([
      listUrl,
      listSinceUrl(NO_ROWS_UPDATED_AT),
      listSinceUrl(NO_ROWS_UPDATED_AT),
    ]);
    expect(rig.backoffs).toEqual([]);
  });

  test.each([401, 403])("a %d from Dispatch is unhealthy, never retried", async (status) => {
    const rig = harness([{ key: "LEGSMOKE-1", parent: null }], [row(100, "LEGSMOKE-1", 1)]);
    rig.dispatch.failure = () => Response.json({ code: "FORBIDDEN" }, { status });

    await expect(rig.run(2)).rejects.toThrow(`Dispatch answered ${status}`);

    expect(rig.backoffs).toEqual([]);
    expect(rig.published).toEqual([]);
    expect(rig.drained).toBe(true);
  });

  test("a root issue absent from the project's issues is unhealthy, never retried", async () => {
    const rig = harness([{ key: "LEGSMOKE-2", parent: null }], []);

    await expect(rig.run(2)).rejects.toThrow(
      "root issue LEGSMOKE-1 is not an issue of project LEGSMOKE on http://dispatch.test"
    );

    expect(rig.backoffs).toEqual([]);
    expect(rig.dispatch.requests).toEqual([listUrl]);
  });
});

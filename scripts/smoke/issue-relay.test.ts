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
  orderEvents,
  POLL_INTERVAL_MS,
  RETRY_CAP_MS,
  RETRY_INITIAL_MS,
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

describe("orderEvents", () => {
  test("interleaves every key's rows by global event id", () => {
    const row = (id: number, key: string, seq: number): DispatchEventRow => ({
      ...(issueCreatedRoot as DispatchEventRow),
      id,
      issue_key: key,
      seq,
    });
    const ordered = orderEvents([
      [row(10, "LEGSMOKE-1", 1), row(13, "LEGSMOKE-1", 2)],
      [row(11, "LEGSMOKE-2", 1), row(12, "LEGSMOKE-2", 2)],
    ]);
    expect(ordered.map((entry) => entry.id)).toEqual([10, 11, 12, 13]);
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

const row = (id: number, key: string, seq: number, type = "issue.updated"): DispatchEventRow => ({
  id,
  issue_key: key,
  artifact_id: null,
  project: "LEGSMOKE",
  seq,
  type,
  actor: { kind: "session", id: "legion-daemon:LEGSMOKE" },
  notify: false,
  created_at: "2026-09-13T10:00:00.000000Z",
  payload: { key, parent: null, status: "todo" },
});

function fakeDispatch(issues: FakeIssue[], events: DispatchEventRow[]): FakeDispatch {
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
        const parent = parsed.searchParams.get("parent");
        return Response.json(
          dispatch.issues
            .filter((issue) => issue.parent === parent)
            .map((issue) => ({ key: issue.key, parent: issue.parent, status: "todo" }))
        );
      }
      const issueMatch = parsed.pathname.match(/^\/api\/v1\/issues\/([^/]+)$/);
      if (issueMatch) {
        const issue = dispatch.issues.find((candidate) => candidate.key === issueMatch[1]);
        return issue
          ? Response.json({ key: issue.key, parent: issue.parent })
          : Response.json({ code: "NOT_FOUND" }, { status: 404 });
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
  run: (ticks: number) => Promise<void>;
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
  const state: Harness = {
    dispatch,
    published,
    log,
    backoffs,
    connections: 0,
    drained: false,
    run: async (ticks: number) => {
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
        // interval (whose 2000 ms coincides with the second backoff step).
        sleep: async (ms: number) => {
          if (log.at(-1)?.startsWith("RELAY RETRY")) {
            backoffs.push(ms);
            return;
          }
          expect(ms).toBe(POLL_INTERVAL_MS);
          polls += 1;
          if (polls >= ticks) controller.abort();
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

describe("runRelay", () => {
  test("discovers the root's children, publishes every key's rows in global id order once, and only asks for newer rows afterwards", async () => {
    const rig = harness(
      [
        { key: "LEGSMOKE-1", parent: null },
        { key: "LEGSMOKE-2", parent: "LEGSMOKE-1" },
        { key: "LEGSMOKE-3", parent: "LEGSMOKE-2" },
        { key: "LEGSMOKE-99", parent: null },
      ],
      [
        row(100, "LEGSMOKE-1", 1, "issue.created"),
        row(101, "LEGSMOKE-2", 1, "issue.created"),
        row(102, "LEGSMOKE-1", 2, "child.status"),
        row(103, "LEGSMOKE-3", 1, "issue.created"),
        row(104, "LEGSMOKE-99", 1, "issue.created"),
        row(105, "LEGSMOKE-2", 2),
      ]
    );

    await rig.run(2);

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
    expect(rig.log.filter((line) => line.startsWith("RELAY TRACKING"))).toEqual([
      "RELAY TRACKING LEGSMOKE-2 parent=LEGSMOKE-1",
      "RELAY TRACKING LEGSMOKE-3 parent=LEGSMOKE-2",
    ]);
    expect(rig.log.filter((line) => line.startsWith("RELAY READY"))).toEqual([
      "RELAY READY root=LEGSMOKE-1 project=LEGSMOKE dispatch=http://dispatch.test downstream=nats://127.0.0.1:14222",
    ]);
    expect(rig.log.filter((line) => line.startsWith("RELAYED"))).toEqual([
      "RELAYED subject=notifications.dispatch.issue.LEGSMOKE-1.issue.created event=dispatch-100 seq=1 stream_seq=1 duplicate=false",
      "RELAYED subject=notifications.dispatch.issue.LEGSMOKE-2.issue.created event=dispatch-101 seq=1 stream_seq=2 duplicate=false",
      "RELAYED subject=notifications.dispatch.issue.LEGSMOKE-1.child.status event=dispatch-102 seq=2 stream_seq=3 duplicate=false",
      "RELAYED subject=notifications.dispatch.issue.LEGSMOKE-3.issue.created event=dispatch-103 seq=1 stream_seq=4 duplicate=false",
      "RELAYED subject=notifications.dispatch.issue.LEGSMOKE-2.issue.updated event=dispatch-105 seq=2 stream_seq=5 duplicate=false",
    ]);
    // Nothing for another rig's root (LEGSMOKE-99) was ever fetched.
    expect(rig.dispatch.requests.some((url) => url.includes("LEGSMOKE-99"))).toBe(false);
    // The second tick asks each key only for rows newer than its acked cursor.
    const secondTickEventRequests = rig.dispatch.requests
      .filter((url) => url.includes("/events?"))
      .slice(-3);
    expect(secondTickEventRequests).toEqual([
      `http://dispatch.test/api/v1/issues/LEGSMOKE-1/events?after=2&limit=${EVENT_PAGE_LIMIT}`,
      `http://dispatch.test/api/v1/issues/LEGSMOKE-2/events?after=2&limit=${EVENT_PAGE_LIMIT}`,
      `http://dispatch.test/api/v1/issues/LEGSMOKE-3/events?after=1&limit=${EVENT_PAGE_LIMIT}`,
    ]);
    expect(rig.connections).toBe(1);
    expect(rig.drained).toBe(true);
  });

  test("pages a key whose backlog exceeds one page", async () => {
    const backlog = Array.from({ length: EVENT_PAGE_LIMIT + 3 }, (_, index) =>
      row(1000 + index, "LEGSMOKE-1", index + 1)
    );
    const rig = harness([{ key: "LEGSMOKE-1", parent: null }], backlog);

    await rig.run(1);

    expect(rig.published).toHaveLength(EVENT_PAGE_LIMIT + 3);
    expect(rig.dispatch.requests.filter((url) => url.includes("/events?"))).toEqual([
      `http://dispatch.test/api/v1/issues/LEGSMOKE-1/events?after=0&limit=${EVENT_PAGE_LIMIT}`,
      `http://dispatch.test/api/v1/issues/LEGSMOKE-1/events?after=${EVENT_PAGE_LIMIT}&limit=${EVENT_PAGE_LIMIT}`,
    ]);
  });

  test("retries a Dispatch failure with doubling backoff, advances no cursor, then publishes the missed rows and recovers", async () => {
    const rig = harness([{ key: "LEGSMOKE-1", parent: null }], [row(100, "LEGSMOKE-1", 1)]);
    let failures = 0;
    rig.dispatch.failure = (url) => {
      // Tick 2 onwards: the events read fails seven times (network, 502, then non-JSON bodies)
      // while a new row lands on Dispatch, unseen until the read succeeds again.
      if (!url.includes("/events?") || rig.published.length === 0 || failures >= 7)
        return undefined;
      failures += 1;
      if (failures === 1) {
        rig.dispatch.events.push(row(101, "LEGSMOKE-1", 2));
        return new Error("connect ECONNREFUSED");
      }
      if (failures === 2) return new Response("<html>bad gateway</html>", { status: 502 });
      return new Response("not json", { status: 200 });
    };

    await rig.run(3);

    expect(rig.backoffs).toEqual([1000, 2000, 4000, 8000, 16000, 30000, 30000]);
    expect(RETRY_INITIAL_MS).toBe(1000);
    expect(RETRY_CAP_MS).toBe(30000);
    const retries = rig.log.filter((line) => line.startsWith("RELAY RETRY"));
    expect(retries).toHaveLength(7);
    expect(retries[0]).toMatch(/^RELAY RETRY attempt=1 next=1000ms: .*ECONNREFUSED/);
    expect(retries[1]).toMatch(/^RELAY RETRY attempt=2 next=2000ms: .*502/);
    expect(retries[6]).toMatch(/^RELAY RETRY attempt=7 next=30000ms: /);
    expect(rig.log.filter((line) => line.startsWith("RELAY RECOVERED"))).toEqual([
      "RELAY RECOVERED after 7 attempts",
    ]);
    // Every retried read re-asked from the last acked cursor; the missed row was published once.
    expect(
      rig.dispatch.requests.filter((url) => url.endsWith("/events?after=1&limit=200"))
    ).toHaveLength(8);
    expect(rig.published.map((entry) => entry.envelope.event_id)).toEqual([
      "dispatch-100",
      "dispatch-101",
    ]);
    expect(rig.log.filter((line) => line.startsWith("RELAY READY"))).toHaveLength(1);
  });

  test("a rejected publish is retried without skipping the row", async () => {
    const rig = harness([{ key: "LEGSMOKE-1", parent: null }], [row(100, "LEGSMOKE-1", 1)]);
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
            if (publishes === 1) throw new Error("nats: timeout");
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
    expect(published).toEqual(["notifications.dispatch.issue.LEGSMOKE-1.issue.updated"]);
    expect(rig.log.filter((line) => line.startsWith("RELAY RETRY"))).toEqual([
      "RELAY RETRY attempt=1 next=1000ms: nats: timeout",
    ]);
    // READY waits for the first tick that fully succeeds, publishes included.
    expect(rig.log.indexOf("RELAY RETRY attempt=1 next=1000ms: nats: timeout")).toBeLessThan(
      rig.log.findIndex((line) => line.startsWith("RELAY READY"))
    );
  });

  test("a root with no events yet is READY once, publishes nothing, and keeps polling", async () => {
    const rig = harness([{ key: "LEGSMOKE-1", parent: null }], []);

    await rig.run(3);

    expect(rig.published).toEqual([]);
    expect(rig.log.filter((line) => line.startsWith("RELAY READY"))).toHaveLength(1);
    expect(rig.dispatch.requests.filter((url) => url.includes("/events?"))).toHaveLength(3);
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

  test("a root issue Dispatch does not know is unhealthy, never retried", async () => {
    const rig = harness([{ key: "LEGSMOKE-2", parent: null }], []);

    await expect(rig.run(2)).rejects.toThrow("root issue LEGSMOKE-1 does not exist");

    expect(rig.backoffs).toEqual([]);
    expect(rig.dispatch.requests).toEqual(["http://dispatch.test/api/v1/issues/LEGSMOKE-1"]);
  });
});

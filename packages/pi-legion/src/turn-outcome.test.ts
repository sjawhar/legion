import { expect, test } from "bun:test";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { AgentEndEvent, AgentEndMessage } from "@legion/pi-shared/pi-types";
import { createTurnOutcomeReporter } from "./turn-outcome";

/**
 * The reporter against the real daemon client over a fake `fetch`: the body the daemon would read
 * is what each test asserts on, since the class and the sequence are the whole report.
 */

const SESSION = "ses_implementer_43";

function capability(generation = 5): ClaimCapability {
  return {
    sessionID: SESSION,
    claimToken: "legion-REPO-43-implementer",
    generation,
    tree: "REPO-42",
    issue: "REPO-43",
    role: "implementer",
    secret: "U3VwZXJ2aXNlZEJ5TGVnaW9u",
  };
}

/** A turn whose last assistant message ended the way `ending` says. */
function turn(ending: AgentEndMessage): AgentEndEvent {
  return {
    messages: [{ role: "user" }, { role: "assistant", stopReason: "stop" }, ending],
  };
}

const CLEAN = turn({ role: "assistant", stopReason: "stop" });
const FILTERED = turn({
  role: "assistant",
  stopReason: "error",
  errorMessage: "Refusal (reasoning_extraction): I cannot help with that",
});
const OVERLOADED = turn({
  role: "assistant",
  stopReason: "error",
  errorMessage: "529 Overloaded",
});

function fixture(options: { readonly claim?: ClaimCapability; readonly status?: number } = {}) {
  const held = options.claim === undefined ? capability() : options.claim;
  const sent: Record<string, unknown>[] = [];
  const warnings: { message: string; context?: Record<string, unknown> }[] = [];
  const debugs: { message: string; context?: Record<string, unknown> }[] = [];
  const status = options.status ?? 204;
  const fetchStub = (async (_url: string, init?: RequestInit) => {
    sent.push(JSON.parse(String(init?.body)));
    return status === 204
      ? new Response(null, { status })
      : new Response(JSON.stringify({ error: "the daemon could not record it" }), { status });
  }) as typeof fetch;
  const daemon = createLegionDaemonClient("http://daemon.test", fetchStub);
  let claim: ClaimCapability | undefined = held;
  const reporter = createTurnOutcomeReporter({
    daemon: () => daemon,
    claim: (sessionID) => (sessionID === SESSION ? claim : undefined),
    log: {
      debug: (message, context) => debugs.push({ message, context }),
      warn: (message, context) => warnings.push({ message, context }),
    },
  });
  return {
    reporter,
    sent,
    warnings,
    debugs,
    setClaim: (next: ClaimCapability | undefined) => {
      claim = next;
    },
  };
}

test("a turn the provider ended cleanly is reported clean", async () => {
  const f = fixture();

  await f.reporter.report(SESSION, CLEAN);

  expect(f.sent).toEqual([
    {
      claimToken: "legion-REPO-43-implementer",
      sessionId: SESSION,
      secret: "U3VwZXJ2aXNlZEJ5TGVnaW9u",
      generation: 5,
      sequence: 1,
      outcome: "clean",
    },
  ]);
  expect(f.warnings).toEqual([]);
  expect(f.debugs.length).toBe(1);
});

test("a content filter is reported refused, with the category and no provider text", async () => {
  const f = fixture();

  await f.reporter.report(SESSION, FILTERED);

  expect(f.sent[0]).toMatchObject({
    sequence: 1,
    outcome: "refused",
    category: "reasoning_extraction",
  });
  // The provider's own sentence reaches no field of the report.
  expect(JSON.stringify(f.sent[0])).not.toContain("I cannot help");
});

test("any other provider failure is reported provider-error", async () => {
  const f = fixture();

  await f.reporter.report(SESSION, OVERLOADED);

  expect(f.sent[0]).toMatchObject({ sequence: 1, outcome: "provider-error" });
  expect(f.sent[0]).not.toHaveProperty("category");
});

test("a turn the host will retry itself reports nothing and takes no sequence", async () => {
  const f = fixture();

  await f.reporter.report(SESSION, { ...FILTERED, willContinue: true });
  await f.reporter.report(SESSION, CLEAN);

  expect(f.sent).toHaveLength(1);
  expect(f.sent[0]).toMatchObject({ sequence: 1, outcome: "clean" });
});

test("the sequence counts the turns of a generation and starts again at the next", async () => {
  const f = fixture();

  await f.reporter.report(SESSION, CLEAN);
  await f.reporter.report(SESSION, FILTERED);
  f.setClaim(capability(6));
  await f.reporter.report(SESSION, CLEAN);

  expect(f.sent.map((body) => [body.generation, body.sequence])).toEqual([
    [5, 1],
    [5, 2],
    [6, 1],
  ]);
});

test("a session holding no claim reports nothing", async () => {
  const f = fixture();

  await f.reporter.report("ses_someone_else", CLEAN);
  f.setClaim(undefined);
  await f.reporter.report(SESSION, FILTERED);

  expect(f.sent).toEqual([]);
});

test("a send the daemon refuses is warned about rather than thrown", async () => {
  const f = fixture({ status: 500 });

  await f.reporter.report(SESSION, FILTERED);

  expect(f.warnings).toHaveLength(1);
  expect(f.warnings[0]?.message).toBe("legion: a turn's outcome did not reach the daemon");
  expect(f.warnings[0]?.context).toMatchObject({
    issue: "REPO-43",
    sequence: 1,
    outcome: "refused",
  });
  expect(f.debugs).toEqual([]);
});

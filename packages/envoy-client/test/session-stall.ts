/**
 * The committed regression rig for the three session states (dispatch://INFRA-392): a session
 * whose box stalls it is `unresponsive`, not dead, and keeps its record, its role claim and its
 * messages until the gone horizon.
 *
 * It runs no agent, so it runs where CI runs: no model route and no agent process. The stalled
 * session is a child Bun process of this file (`runSessionChild`) carrying this package's real
 * registration and heartbeat — `createEnvoyClient(...).subscribe`, the call every plugin's
 * heartbeat repeats (`registerSession`, packages/pi-envoy/extensions/envoy.ts), with the real
 * role claim and the real `POST /v1/replay` read beside it — against the real listener and real
 * natsd test/envoy-stack.ts builds from this checkout. The stall is `SIGSTOP`, which stops
 * exactly what a loaded box stops: the heartbeat, with nothing closed and nothing deregistered.
 * `SIGCONT` resumes it and `SIGKILL` ends it with no deregistration, as a killed pane leaves it.
 *
 * Run it from packages/envoy-client with `bun test ./test/session-stall.ts` and
 * LEGION_E2E_ENVOY_STACK=1 (the envoy-session-stall job sets it).
 */

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { agentSubject, EnvelopeSchema } from "@legion/contracts";
import { connect, type KV, type NatsConnection } from "nats";
import { z } from "zod";
import { createEnvoyClient, type EnvoyClient, type SessionState } from "../src/transport";
import { type EnvoyStack, envoyStackEnabled, startEnvoyStack } from "./envoy-stack";

// Destructured: claude-envoy's typecheck, which compiles this directory, forbids `process.env.X`.
const { ENVOY_SESSION_STALL_CHILD } = process.env;

/**
 * The windows the listener runs with here, the deployment's shape in seconds instead of hours so
 * that crossing each one costs the test its real time and no more. The ratio is the deployed one:
 * the child re-registers five times inside the live window, as a pane's 90 s heartbeat does
 * inside the five-minute window (`session.LiveWindow`, packages/envoy/internal/session/state.go).
 */
const HEARTBEAT_MS = 2_000;
const LIVE_WINDOW_MS = 10_000;
const GONE_HORIZON_MS = 60_000;
const REAP_INTERVAL_MS = 2_000;
/**
 * The session bucket's maximum age, far above the gone horizon. It is the setting that makes the
 * horizon mean anything: the bucket drops a record this long after the last registration whatever
 * the horizon says, so an age below the horizon would take a stalled session's record away before
 * it could be read as unresponsive — which reads as `gone`, the very bug this rig exists for.
 */
const SESSION_MAX_AGE_MS = 30 * 60_000;

/**
 * How often the listener's role claim reaper runs: the one loop that releases a gone holder's
 * claim, on the hard-coded cadence of its every KV loop (`StartRoleClaimReaper`,
 * packages/envoy/cmd/listener/main.go). Nothing shortens it, so the last property below waits out
 * one pass of it in real time.
 */
const ROLE_REAP_INTERVAL_MS = 5 * 60_000;

/** The gap allowed between a registration the child reports and the `last_seen` the listener
 *  stamped for it: one request's latency, plus room for a registration the freeze cut off
 *  between the listener's write and the child's own line. */
const REGISTRATION_SLACK_MS = 2_000;

const POLL_MS = 200;
const ROLE_CLAIM_BUCKET = "envoy_roles";
const OBSERVER_SESSION = "ses_session_stall_observer";
const PLUGIN = { name: "@legion/envoy-client", version: "session-stall-rig" };

/** A Go duration for the listener's environment: milliseconds are a Go duration unit. */
const goDuration = (ms: number): string => `${ms}ms`;

const StateSchema = z.enum(["live", "unresponsive", "gone"]);

const SessionRowSchema = z.object({
  session_id: z.string(),
  state: StateSchema,
  last_seen: z.number().int(),
});

/** The 404 of a session that is not live: the kept `no live session <id>` wording, with the
 *  state and the last registration the refusal was decided on beside it. */
const NoLiveSessionSchema = z.object({
  error: z.string(),
  state: StateSchema,
  last_seen: z.number().int().optional(),
});

const RoleHolderSchema = z.object({
  role: z.string(),
  holder: z.string(),
  last_seen: z.number().int(),
  holder_state: StateSchema.optional(),
});

const RoleRefusalSchema = z.object({
  error: z.string(),
  reason: z.string(),
  holder: z.string().optional(),
  holder_state: StateSchema.optional(),
  last_seen: z.number().int().optional(),
  claim_released: z.boolean().optional(),
});

/** One role claim as the `envoy_roles` bucket holds it. `claimed_at` is readable nowhere else:
 *  no route answers it, which is why this rig reads the bucket to prove a stall moved neither it
 *  nor the holder. */
const RoleClaimSchema = z.object({
  holder_session_id: z.string(),
  claimed_at: z.number().int(),
  previous_session_id: z.string().optional(),
});

const ChildConfigSchema = z.object({
  envoyUrl: z.string(),
  sessionID: z.string(),
  role: z.string(),
  directory: z.string(),
  title: z.string(),
  heartbeatMs: z.number().int().positive(),
});

const ChildEventSchema = z.discriminatedUnion("event", [
  z.object({ event: z.literal("registered"), at: z.number().int() }),
  z.object({ event: z.literal("role"), claimed: z.boolean() }),
  z.object({ event: z.literal("message"), text: z.string(), seq: z.number().int() }),
  z.object({ event: z.literal("failed"), what: z.string(), error: z.string() }),
]);

type ChildConfig = z.infer<typeof ChildConfigSchema>;
type ChildEvent = z.infer<typeof ChildEventSchema>;
type RoleClaim = z.infer<typeof RoleClaimSchema>;

/**
 * The stalled session itself, run as a child process of this file.
 *
 * Everything it does on the wire is this package's own: `createEnvoyClient(...).subscribe` is the
 * registration, repeated on an interval, which is what a plugin's heartbeat is (pi-envoy's
 * `registerSession` sends this very body: port 0, its own agent subject followed, self-subscribed,
 * the plugin named); `setRole` is the real role claim; `replay` is the real read of what the
 * stream stored for it, the path a self-subscribed session takes its messages back on after it
 * was away. Nothing here fakes a listener call.
 *
 * It says each accepted registration and each message it read back on stdout, one JSON object per
 * line, and never returns: the parent freezes, continues and kills it.
 */
async function runSessionChild(config: ChildConfig): Promise<never> {
  const say = (event: ChildEvent): void => {
    process.stdout.write(`${JSON.stringify(event)}\n`);
  };
  const client = createEnvoyClient({ baseUrl: config.envoyUrl, fetch });
  const subject = agentSubject(config.sessionID);
  const register = async (): Promise<void> => {
    await client.subscribe({
      sessionID: config.sessionID,
      directory: config.directory,
      topics: [subject],
      port: 0,
      title: config.title,
      driving: false,
      selfSubscribed: true,
      plugin: PLUGIN,
    });
    say({ event: "registered", at: Date.now() });
  };
  // Where the next replay reads from. The first call names no cursor, which answers no messages
  // and the stream's watermark, so what the rig sends later is read from there on — including
  // what was stored while this process was frozen.
  let after: number | undefined;
  const readStored = async (): Promise<void> => {
    const answer = await client.replay({
      sessionID: config.sessionID,
      subjects: [after === undefined ? { subject } : { subject, after }],
    });
    const followed = answer.subjects.find((entry) => entry.subject === subject);
    if (followed === undefined) return;
    for (const stored of followed.messages) {
      say({
        event: "message",
        text: EnvelopeSchema.parse(JSON.parse(stored.data)).payload_summary,
        seq: stored.seq,
      });
    }
    after = followed.next_after;
  };

  await register();
  const claim = await client.setRole({ sessionID: config.sessionID, role: config.role });
  say({ event: "role", claimed: claim.claimed });
  await readStored();
  setInterval(() => {
    void (async () => {
      try {
        await register();
        await readStored();
      } catch (error) {
        // A pass the freeze cut in half fails once on resume; the next tick registers again,
        // exactly as a pane's heartbeat does after its box comes back.
        say({ event: "failed", what: "a heartbeat pass", error: String(error) });
      }
    })();
  }, config.heartbeatMs);
  return new Promise<never>(() => undefined);
}

if (ENVOY_SESSION_STALL_CHILD !== undefined) {
  // A child runs the session above until it is killed and never reaches the suite below.
  await runSessionChild(ChildConfigSchema.parse(JSON.parse(ENVOY_SESSION_STALL_CHILD)));
}

/** A session process the rig can stall, as the parent holds it. */
interface StalledSession {
  readonly sessionID: string;
  readonly role: string;
  /** When the child's last accepted registration landed, by its own clock. */
  registeredAt(): number;
  /** The `payload_summary` of every message it has read back, in arrival order. */
  messages(): readonly string[];
  /** Freezes it as a loaded box does: no heartbeat, no reads, no socket closed, no
   *  deregistration. Answers the moment of the freeze. */
  freeze(): number;
  /** Continues it. */
  thaw(): void;
  /** Kills it outright, so nothing deregisters it: what a pane whose process is killed leaves. */
  kill(): Promise<void>;
  /** Whatever it has written to stderr, for an error that names why it stopped registering. */
  stderr(): string;
}

/** Starts the child and resolves once the listener has accepted its registration and its claim. */
async function startStalledSession(config: ChildConfig): Promise<StalledSession> {
  const child = Bun.spawn(["bun", "run", import.meta.path], {
    env: { ...process.env, ENVOY_SESSION_STALL_CHILD: JSON.stringify(config) },
    stdout: "pipe",
    stderr: "pipe",
  });
  let registeredAt = 0;
  let claimed = false;
  const messages: string[] = [];
  let errors = "";
  const read = async (
    stream: ReadableStream<Uint8Array>,
    onLine: (line: string) => void
  ): Promise<void> => {
    const decoder = new TextDecoder();
    let rest = "";
    for await (const chunk of stream) {
      const lines = (rest + decoder.decode(chunk, { stream: true })).split("\n");
      rest = lines.pop() ?? "";
      for (const line of lines) onLine(line);
    }
  };
  void read(child.stdout, (line) => {
    if (line.trim() === "") return;
    const event = ChildEventSchema.parse(JSON.parse(line));
    if (event.event === "registered") registeredAt = event.at;
    if (event.event === "role") claimed = event.claimed;
    if (event.event === "message") messages.push(event.text);
    if (event.event === "failed") errors += `${event.what}: ${event.error}\n`;
  }).catch((error: unknown) => {
    errors += `reading the session's output: ${String(error)}\n`;
  });
  void read(child.stderr, (line) => {
    errors += `${line}\n`;
  }).catch(() => undefined);
  const session: StalledSession = {
    sessionID: config.sessionID,
    role: config.role,
    registeredAt: () => registeredAt,
    messages: () => messages,
    freeze: () => {
      child.kill("SIGSTOP");
      return Date.now();
    },
    thaw: () => child.kill("SIGCONT"),
    kill: async () => {
      // Idempotent: the last property kills it, and the suite's teardown asks again.
      if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
      await child.exited;
    },
    stderr: () => errors,
  };
  const deadline = Date.now() + 30_000;
  while (registeredAt === 0 || !claimed) {
    if (child.exitCode !== null || Date.now() > deadline) {
      await session.kill();
      throw new Error(`the stalled session did not register and claim ${config.role}: ${errors}`);
    }
    await Bun.sleep(POLL_MS);
  }
  return session;
}

/** What `GET /v1/registry/<id>?include=unresponsive` says about one session. */
interface Sighting {
  readonly state: SessionState;
  /** Its last registration (Unix ms), 0 when the listener remembers none. */
  readonly lastSeen: number;
}

async function sighting(envoyUrl: string, sessionID: string): Promise<Sighting> {
  const path = `/v1/registry/${encodeURIComponent(sessionID)}?include=unresponsive`;
  const response = await fetch(`${envoyUrl}${path}`);
  const body: unknown = await response.json();
  if (response.ok) {
    const row = SessionRowSchema.parse(body);
    return { state: row.state, lastSeen: row.last_seen };
  }
  if (response.status !== 404) {
    throw new Error(`GET ${path} answered ${response.status}: ${JSON.stringify(body)}`);
  }
  const refusal = NoLiveSessionSchema.parse(body);
  return { state: refusal.state, lastSeen: refusal.last_seen ?? 0 };
}

/** Polls the session until it reads `want`, throwing with what it did read when `deadline` passes. */
async function awaitState(
  envoyUrl: string,
  sessionID: string,
  want: SessionState,
  deadline: number
): Promise<Sighting> {
  let seen = await sighting(envoyUrl, sessionID);
  while (seen.state !== want) {
    if (Date.now() > deadline) {
      throw new Error(`${sessionID} still reads ${seen.state}, not ${want}, at the deadline`);
    }
    await Bun.sleep(POLL_MS);
    seen = await sighting(envoyUrl, sessionID);
  }
  return seen;
}

/** Polls `condition` until it holds, throwing `what` when `deadline` passes. */
async function until(
  what: string,
  deadline: number,
  condition: () => boolean | Promise<boolean>
): Promise<void> {
  while (!(await condition())) {
    if (Date.now() > deadline) throw new Error(`${what} did not happen before the deadline`);
    await Bun.sleep(POLL_MS);
  }
}

type RoleAnswer =
  | { readonly status: number; readonly holder: z.infer<typeof RoleHolderSchema> }
  | { readonly status: number; readonly refusal: z.infer<typeof RoleRefusalSchema> };

/** `GET /v1/roles/<role>`: its holder, or the 404 naming why the listener routes nothing there. */
async function roleAnswer(envoyUrl: string, role: string): Promise<RoleAnswer> {
  const response = await fetch(`${envoyUrl}/v1/roles/${encodeURIComponent(role)}`);
  const body: unknown = await response.json();
  return response.ok
    ? { status: response.status, holder: RoleHolderSchema.parse(body) }
    : { status: response.status, refusal: RoleRefusalSchema.parse(body) };
}

/** The claim the role bucket holds, or undefined once something released it. */
async function roleClaim(claims: KV, role: string): Promise<RoleClaim | undefined> {
  const entry = await claims.get(role);
  if (entry === null || entry.operation !== "PUT") return undefined;
  return RoleClaimSchema.parse(JSON.parse(entry.string()));
}

describe.skipIf(!envoyStackEnabled)("a session its box stalls", () => {
  let stack: EnvoyStack | undefined;
  let nats: NatsConnection | undefined;
  let claims: KV | undefined;
  let child: StalledSession | undefined;
  let client: EnvoyClient | undefined;

  /** The started rig, so each test reads one checked set of handles. */
  const rig = () => {
    if (
      stack === undefined ||
      claims === undefined ||
      child === undefined ||
      client === undefined
    ) {
      throw new Error("the session-stall rig did not start");
    }
    return { envoyUrl: stack.envoyUrl, claims, child, client };
  };

  beforeAll(async () => {
    const started = await startEnvoyStack({
      listenerEnv: {
        ENVOY_SESSION_LIVE_WINDOW: goDuration(LIVE_WINDOW_MS),
        ENVOY_SESSION_GONE_HORIZON: goDuration(GONE_HORIZON_MS),
        ENVOY_SESSION_REAP_INTERVAL: goDuration(REAP_INTERVAL_MS),
        ENVOY_SESSION_MAX_AGE: goDuration(SESSION_MAX_AGE_MS),
      },
    });
    stack = started;
    client = createEnvoyClient({ baseUrl: started.envoyUrl, fetch });
    child = await startStalledSession({
      envoyUrl: started.envoyUrl,
      sessionID: `ses_stalled_${process.pid}`,
      role: "session-stall-reviewer",
      directory: "/test/session-stall",
      title: "Stalled session",
      heartbeatMs: HEARTBEAT_MS,
    });
    nats = await connect({ servers: [started.natsUrl], name: "session-stall-observer" });
    claims = await nats.jetstream().views.kv(ROLE_CLAIM_BUCKET, { bindOnly: true });
  }, 180_000);

  afterAll(async () => {
    await child?.kill();
    await nats?.close();
    await stack?.stop();
  }, 60_000);

  // Properties 1, 2 and 3. A listener that read a missed heartbeat as death answers this session
  // nothing at all: no state to read, no last_seen to date the stall by, and an `include` that
  // can list nothing, because the record is gone the moment the window passes.
  test("a stopped session reads unresponsive with its last registration, is listed only when the caller asks for it, and SIGCONT returns it to live", async () => {
    const { envoyUrl, child, client } = rig();
    const frozenAt = child.freeze();
    // Let a registration the freeze may have cut between the listener's write and the child's
    // own line land before either is read.
    await Bun.sleep(HEARTBEAT_MS);
    const registered = child.registeredAt();

    const stalled = await awaitState(
      envoyUrl,
      child.sessionID,
      "unresponsive",
      frozenAt + LIVE_WINDOW_MS + 20_000
    );

    // (1) The state is its record's age, and `last_seen` is the registration it made before it
    // froze — not the moment the listener noticed, which is a whole live window later.
    expect(Math.abs(stalled.lastSeen - registered)).toBeLessThan(REGISTRATION_SLACK_MS);
    expect(Date.now() - stalled.lastSeen).toBeGreaterThan(LIVE_WINDOW_MS);

    // (2) The default listing is unchanged for every caller that only ever wanted live
    // sessions; the stalled row is there for a caller that asks, with the same last_seen.
    const byDefault = await client.listSessions();
    expect(byDefault.map((row) => row.session_id)).not.toContain(child.sessionID);
    const asked = await client.listSessions({ include: "unresponsive" });
    const row = asked.find((listed) => listed.session_id === child.sessionID);
    expect(row?.state).toBe("unresponsive");
    expect(row?.last_seen).toBe(stalled.lastSeen);

    // The stall moves nothing: its last registration is still the one it froze on.
    expect((await sighting(envoyUrl, child.sessionID)).lastSeen).toBe(stalled.lastSeen);

    // (3) The box comes back, the heartbeat resumes, and the same session is live again.
    child.thaw();
    const resumed = await awaitState(
      envoyUrl,
      child.sessionID,
      "live",
      Date.now() + HEARTBEAT_MS * 3 + 20_000
    );
    expect(resumed.lastSeen).toBeGreaterThan(stalled.lastSeen);
    expect((await client.listSessions()).map((listed) => listed.session_id)).toContain(
      child.sessionID
    );
  }, 180_000);

  // Properties 4 and 5. A listener that read the stall as death releases this claim on the first
  // lookup and refuses the send with "no live session", so the role goes to whoever asks next and
  // the message is never stored for the session that was only stalled.
  test("a stalled session keeps its role claim through any number of lookups, and a send to it is stored and arrives when it answers again", async () => {
    const { envoyUrl, claims, child, client } = rig();
    const live = `stall-live-${crypto.randomUUID()}`;
    const sentLive = await client.send({
      source: "agent",
      sourceSessionID: OBSERVER_SESSION,
      targetSessionID: child.sessionID,
      message: live,
    });
    expect(sentLive.recipientState).toBe("live");
    await until(`${live} to reach the live session`, Date.now() + 30_000, () =>
      child.messages().includes(live)
    );
    const claimed = await roleClaim(claims, child.role);
    expect(claimed?.holder_session_id).toBe(child.sessionID);

    const frozenAt = child.freeze();
    const stalled = await awaitState(
      envoyUrl,
      child.sessionID,
      "unresponsive",
      frozenAt + LIVE_WINDOW_MS + 20_000
    );

    // (4) Every lookup answers the 404 it always did, now saying whose claim it is, that the
    // holder is stalled rather than gone, and that it released nothing. Five of them, because
    // the release this replaces happened inside the lookup: one used to be enough to lose it.
    for (let lookup = 0; lookup < 5; lookup += 1) {
      const answer = await roleAnswer(envoyUrl, child.role);
      expect(answer.status).toBe(404);
      expect("refusal" in answer ? answer.refusal : undefined).toMatchObject({
        reason: "holder_lapsed",
        holder: child.sessionID,
        holder_state: "unresponsive",
        last_seen: stalled.lastSeen,
        claim_released: false,
      });
    }
    // The claim record itself, holder and claimed_at, is byte for byte the one it made.
    expect(await roleClaim(claims, child.role)).toEqual(claimed);

    // (5) A send to it is taken and stored, naming the state it was taken for, and reaches the
    // session when it answers again — which is what a stalled pane must find waiting for it.
    const missed = `stall-missed-${crypto.randomUUID()}`;
    const sentStalled = await client.send({
      source: "agent",
      sourceSessionID: OBSERVER_SESSION,
      targetSessionID: child.sessionID,
      message: missed,
    });
    expect(sentStalled.recipientState).toBe("unresponsive");
    expect(child.messages()).not.toContain(missed);

    child.thaw();
    await until(
      `${missed}, sent while the session was stalled, to reach it`,
      Date.now() + 60_000,
      () => child.messages().includes(missed)
    );
    await awaitState(envoyUrl, child.sessionID, "live", Date.now() + HEARTBEAT_MS * 3 + 20_000);
    const back = await roleAnswer(envoyUrl, child.role);
    expect("holder" in back ? back.holder.holder : undefined).toBe(child.sessionID);
    expect(await roleClaim(claims, child.role)).toEqual(claimed);
  }, 240_000);

  // Property 6. The horizon is the whole point: a listener that read the missed heartbeats as
  // death would release this claim within the live window — within seconds of a stall a loaded
  // box recovers from — instead of holding it for the hour a session has to come back in.
  test("a killed session stays unresponsive until the gone horizon, then reads gone, and only then does the reaper release its role claim", async () => {
    const { envoyUrl, claims, child } = rig();
    const claimed = await roleClaim(claims, child.role);
    expect(claimed?.holder_session_id).toBe(child.sessionID);

    const killedAt = Date.now();
    await child.kill();

    const stalled = await awaitState(
      envoyUrl,
      child.sessionID,
      "unresponsive",
      killedAt + LIVE_WINDOW_MS + 20_000
    );
    // Past the live window it is unresponsive, not gone, and its claim stands: nothing can yet
    // tell this from a box that will answer again in a minute.
    const whileStalled = await roleAnswer(envoyUrl, child.role);
    expect("refusal" in whileStalled ? whileStalled.refusal : undefined).toMatchObject({
      holder: child.sessionID,
      holder_state: "unresponsive",
      claim_released: false,
    });
    expect(await roleClaim(claims, child.role)).toEqual(claimed);

    await awaitState(
      envoyUrl,
      child.sessionID,
      "gone",
      stalled.lastSeen + GONE_HORIZON_MS + REAP_INTERVAL_MS + 30_000
    );
    // It reached the horizon by its record's age, not by anything the rig did to it.
    expect(Date.now() - stalled.lastSeen).toBeGreaterThan(GONE_HORIZON_MS);
    // Gone, and the claim is still exactly as it was: the session reaper takes the record, and
    // a lookup of a gone holder still releases nothing.
    expect(await roleClaim(claims, child.role)).toEqual(claimed);
    const atGone = await roleAnswer(envoyUrl, child.role);
    expect("refusal" in atGone ? atGone.refusal : undefined).toMatchObject({
      holder: child.sessionID,
      holder_state: "gone",
      claim_released: false,
    });

    // Only the role claim reaper releases it, on its own pass, and only now that the holder is
    // gone. Its cadence is fixed in the listener, so this waits out one whole pass.
    await until(
      "the role claim reaper to release the gone holder's claim",
      Date.now() + ROLE_REAP_INTERVAL_MS + 90_000,
      async () => (await roleClaim(claims, child.role)) === undefined
    );
    const released = await roleAnswer(envoyUrl, child.role);
    expect("refusal" in released ? released.refusal.reason : undefined).toBe("unclaimed");
  }, 480_000);
});

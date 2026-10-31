import { randomUUID } from "node:crypto";
import { ROLE_TOPIC_PREFIX } from "@legion/contracts";
import { messageFor } from "@legion/envoy-client/errors";
import {
  EnvoyApiError,
  type EnvoyClient,
  expandSubscriptionTopics,
  type Interest,
  type SubscribeInput,
} from "@legion/envoy-client/transport";
import type { NatsConnection } from "nats";

/**
 * The registration and role state the Envoy extension's two serial queues write, the queues
 * themselves, each task's deadline, and this extension instance's client instance id.
 *
 * A queued task never re-checks a fence by hand. `run` hands it a {@link TaskHandle}, and every
 * state write, listener request, NATS subscribe and NATS dial it makes after a wait is a method of
 * that handle. The handle refuses the step once the task's deadline has fired, and, for a task
 * bound to a session (`actFor`), once that session stopped being the open session. A refused step
 * writes nothing and ends the task: past the deadline the caller's promise has already rejected
 * with {@link AbandonedTaskError} and the late settlement is swallowed; after the session moved
 * `run` resolves `undefined`, so a pass the host left mid-pass decides nothing and the user sees
 * no new warning.
 *
 * The state is private to this module: callers read it through the accessors below and write it
 * only through a named operation, so there is no shared variable a later edit can write unguarded.
 */

/** A queued task abandoned at its deadline: the queue has moved on, and the task's own
 *  continuation writes nothing when its await finally resolves. */
export class AbandonedTaskError extends Error {}

/**
 * A step the task's handle refused. Module-private: it never leaves `run`, which turns it into the
 * task's outcome — swallowed on an abandoned task's late settlement, `undefined` once the task's
 * session moved.
 */
class StepRefused extends Error {}

/** Why a held role had to be re-asserted at the listener, for the Legion regain hook. */
export type RoleRegainReason = "reclaimed" | "reregistered";

/** Whether a topic served the session's role, and how many roles had ended, at one moment. */
export interface RoleSnapshot {
  readonly roleBound: boolean;
  readonly endsBefore: number;
}

/** What a registration carries, less the client instance id this module mints and sends itself. */
export type RegistrationInput = Omit<SubscribeInput, "clientInstanceID">;

/** The role this process gave up locally while the listener may still name it. */
export interface RoleEnded {
  readonly role: string;
  /** Whether a plugin held notice subjects for it, which the role's end closed. */
  readonly pluginHeld: boolean;
}

/** What a heartbeat's re-assertion of a held role decided. */
export interface RoleReassertion {
  /** False when the hold moved off this role while the listener was read or claimed. */
  readonly decided: boolean;
  /** Set when the Legion regain hook owes a run for this role. */
  readonly regain?: RoleRegainReason;
}

/** The warnings this module writes; `logger` from the host satisfies it. */
export interface RegistrationStateLog {
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

/**
 * Everything the module needs from its host, injected so the package's own unit tests exercise it
 * with plain fakes: no Oh My Pi host type is imported here.
 */
export interface RegistrationStateDeps {
  /** The listener client, read at each call: it throws while Envoy is off for this session, as
   *  `requireEnvoy()` does, and the queues exist whatever the session resolved. */
  readonly client: () => EnvoyClient;
  /** Whether `id` is the session open here, for this instance and the host alike. */
  readonly sessionOpen: (id: string) => boolean;
  /** The deadline each queued task gets, read afresh when that task starts. */
  readonly deadlineMs: () => number;
  /** Tells the pane what a state write decided. */
  readonly notify: (message: string, level: "warning") => void;
  readonly log: RegistrationStateLog;
  /** Writes the durable transcript record of a claim this process landed. */
  readonly appendRoleClaimEntry: (role: string) => void;
  /** Opens a NATS connection, storing nothing: this module closes one a refused task opened. */
  readonly dial: () => Promise<NatsConnection>;
  /** Opens `topic`'s subjects; answers whether any were new. */
  readonly subscribeTopic: (topic: string) => Promise<boolean>;
  /** Closes a subject this module no longer keeps open, the live subscriptions staying with the
   *  extension: a role's end closes every notice subject a plugin opened while it was held. */
  readonly closeSubject: (subject: string) => void;
}

/**
 * The steps a queued task may take. Each refuses before it acts, so no call site re-checks a
 * fence. Refusal throws, which ends the task.
 */
export interface TaskHandle {
  /**
   * Binds this task to the session it acts for, called once as soon as the task learns it. From
   * then on every step refuses after that session stopped being the open session. Binding twice
   * throws: a task acts for one session.
   */
  readonly actFor: (id: string) => void;
  /** Refuses here, and otherwise does nothing: the step for "stop unless this task still decides".
   */
  readonly check: () => void;

  // — registration —
  /** Registers the session at the listener, carrying this instance's client instance id. */
  readonly register: (input: RegistrationInput) => Promise<Interest>;

  // — the session a pass converges to —
  /** Records that this session's agent subject, registration and Dispatch binding are converged. */
  readonly markEstablished: (id: string) => void;
  /** Forgets every agent session but `host`; answers the ones whose subject the caller must close.
   */
  readonly forgetOtherAgentSessions: (host: string) => readonly string[];
  /** Records that this process may have `id`'s agent subject open. */
  readonly markAgentSessionOpen: (id: string) => void;
  /** Records that `id`'s agent subject, stream watch and registered interests are open in full. */
  readonly markInboundOpen: (id: string) => void;
  /** Records that a pass decided the role for `id`. */
  readonly markRoleSettled: (id: string) => void;

  // — the role —
  /**
   * Ends the role this process holds and hands its hold back at the listener. Answers the role it
   * ended, or `undefined` when it held none. With `tellUser`, says so in the pane: a role end
   * after a move no session event announced is one no plugin heard.
   */
  readonly handBackHeldRole: (tellUser: boolean) => Promise<RoleEnded | undefined>;
  /**
   * Settles the holds session events gave up: one of `wanted`, under a session this process still
   * registers, comes back unconfirmed; every other is handed back, and one the listener refuses is
   * abandoned. Answers whether a hold came back.
   */
  readonly settleEndedHolds: (wanted: string | undefined) => Promise<boolean>;
  /**
   * Claims `role` for this task's session. A soft claim is granted only when the role is unheld,
   * its holder is no longer live, or its holder is `previousSessionID` and that predecessor is
   * this client instance's; a hard claim is last-claim-wins. Answers whether the claim landed.
   *
   * A claim the listener granted after this task's deadline fired is left as a pending hand-back,
   * and one granted after the session moved is adopted under the id the listener names.
   */
  readonly claimRole: (
    role: string,
    options?: { readonly soft?: boolean; readonly previousSessionID?: string }
  ) => Promise<boolean>;
  /**
   * Makes sure the listener still resolves this session as `role`'s holder: reads first, and a
   * soft claim goes out only when it does not. A refusal ends the role here and records it.
   */
  readonly reassertRole: (role: string, afterOutage: boolean) => Promise<RoleReassertion>;
  /** Says, once, that the notices a plugin opened for `role` stay closed, and forgets that it
   *  opened them. Writes nothing while that plugin's subjects are still open. */
  readonly noteNoticesClosed: (role: string) => void;

  // — listener reads and hand-backs —
  /** The session the listener names as `role`'s holder; `undefined` when it names none or the
   *  read failed. */
  readonly readRoleHolder: (role: string) => Promise<string | undefined>;
  /** The listener's registered interests for a session; `undefined` when the read failed. */
  readonly readInterest: (sessionID: string) => Promise<Interest | undefined>;
  /** Hands one hold back at the listener; throws when the listener refuses it. */
  readonly handBack: (topic: string, sessionID: string) => Promise<void>;

  // — NATS —
  /** Opens a NATS connection. One a refused task opened is closed here and never answered. */
  readonly dial: () => Promise<NatsConnection>;
  /** Opens `topic`'s subjects; answers whether any were new. */
  readonly subscribe: (topic: string) => Promise<boolean>;
}

/** One of the module's serial queues. */
export interface TaskQueue {
  /**
   * Runs tasks one at a time, in the order they were queued; a task that fails does not stop the
   * ones after it. Each task gets `deadlineMs()` milliseconds, read when that task starts, after
   * which the caller's promise rejects with {@link AbandonedTaskError}, the queue starts the next
   * task at once, and the abandoned task's own settlement is swallowed. A task a refused step
   * ended because its session moved resolves `undefined`.
   */
  readonly run: <T>(task: (handle: TaskHandle) => Promise<T>) => Promise<T | undefined>;
  /** Resolves once every task queued so far has finished. */
  readonly settled: () => Promise<unknown>;
}

/** The module's surface: the two queues, the named operations, and the readers of its state. */
export interface RegistrationState {
  /**
   * This extension instance's client instance id, minted here so an ACP host running several
   * top-level sessions in one process gives each its own. Sent on every registration and on the
   * shutdown deregistration, where the listener compares it.
   */
  readonly clientInstanceID: string;
  /** Reconcile passes: one at a time, each converging this process to the host's session. */
  readonly passes: TaskQueue;
  /** Role moves: the model's `envoy_role_set`, a plugin's claim, and each pass's own settle. */
  readonly roleMoves: TaskQueue;

  // — named operations, made on purpose without a live task —
  /**
   * Registers the session at the listener, outside any queue. Every hold this process abandoned
   * under that session becomes a pending hand-back under the same key first, because the
   * registration moves the session's `last_seen` at the listener even when its answer is lost. On
   * the answer, that session is the one registered and the registration clock is now.
   */
  readonly refreshRegistration: (input: RegistrationInput) => Promise<Interest>;
  /** Whether a registration this process made for `sessionID` was accepted less than `ms` ago. A
   *  registration accepted for another session, or none at all, reads as older than any age. */
  readonly registrationFresherThan: (sessionID: string, ms: number) => boolean;
  /** Deregisters the session at the listener, carrying this instance's client instance id so a
   *  successor instance's registration under the same id is left alone. */
  readonly deregister: (sessionID: string) => Promise<void>;
  /** Forgets the role the listener refused to this session, so a pass may claim it again. */
  readonly clearRefusal: (sessionID: string) => void;
  /**
   * Ends, as a session event lands, the role this process holds when the host session's own
   * records rule it out: a recorded release, another role, or no record while the hold is not
   * under `host` or a session `host` continues (`lineage`). The hold becomes a pending hand-back
   * under the id it was held as.
   */
  readonly endUnwantedRole: (
    host: string,
    recordedRole: string | null | undefined,
    lineage: readonly string[]
  ) => void;
  /** Marks subjects as taken only while this session holds `role`; answers the ones newly marked.
   */
  readonly markRoleBoundSubjects: (subjects: readonly string[], role: string) => readonly string[];
  /** Unmarks subjects whose subscription never opened. */
  readonly unmarkRoleBoundSubjects: (subjects: readonly string[]) => void;
  /** Records a claim the listener granted after its task was abandoned: the hold is real at the
   *  listener, so the next pass hands it back. */
  readonly recordClaimAfterDeadline: (topic: string, sessionID: string) => void;
  /**
   * Records a claim the listener granted after its task's session moved: the new hold is this
   * process's own under the id the listener names, and an earlier role ends and is handed back.
   */
  readonly adoptClaimAfterSessionMoved: (
    topic: string,
    sessionID: string,
    previousTopic: string | undefined,
    previousHoldID: string | undefined
  ) => Promise<void>;

  // — readers —
  /** The role this session holds, if any. */
  readonly heldRole: () => string | undefined;
  /** Whether `topic` is the role topic this session holds. */
  readonly holdsRoleTopic: (topic: string) => boolean;
  /** The session id the listener names for the role this session holds. */
  readonly holdSessionID: () => string | undefined;
  /** Whether `subject` is taken only while this session holds its role. */
  readonly isRoleBound: (subject: string) => boolean;
  /** Every role-bound subject, in the order they were marked. */
  readonly roleBoundSubjects: () => readonly string[];
  /** Whether `topic` serves the role this session holds, and how many roles had ended, at once. */
  readonly roleSnapshot: (topic: string) => RoleSnapshot;
  /** The role the listener refused to this session's automatic claim, if any. */
  readonly refusalFor: (sessionID: string) => string | undefined;
  /** How many roles have ended, which a role-bound subscribe compares against. */
  readonly roleEndCount: () => number;
  /** Whether the hold came back from a hand-back rather than from a claim the listener granted. */
  readonly holdUnconfirmed: () => boolean;
  /** The session the last pass converged to in full. */
  readonly establishedSession: () => string;
  /** The session whose agent subject, stream watch and registered interests a pass opened. */
  readonly inboundOpenFor: () => string;
  /** The session whose role the last pass decided. */
  readonly roleSettledFor: () => string;
}

export function createRegistrationState(deps: RegistrationStateDeps): RegistrationState {
  const clientInstanceID = randomUUID().replaceAll("-", "");

  // The one role this process holds at the listener, and the session id the listener names for
  // it, which a pass that keeps the role for another session supersedes by naming it.
  let claimedRoleTopic: string | undefined;
  let claimedRoleSessionID: string | undefined;
  // Whether that hold came back from a hand-back a session event left rather than from a claim the
  // listener granted: nothing has checked it at the listener since, so a pass claims it softly,
  // naming it, before trusting it.
  let holdUnconfirmed = false;
  // Holds this process gave up locally while the listener still names them, keyed by
  // `<topic>@<session>`: a session event that ends the role at once leaves its hold here, and the
  // next pass hands it back.
  const pendingHandBacks = new Map<string, { readonly topic: string; readonly id: string }>();
  // Holds this process gave up whose hand-back the listener refused, whatever session they are
  // under, keyed as `pendingHandBacks` is. Any registration this process makes of a hold's session
  // first takes it back as a pending hand-back; until then it is left for the listener to reap.
  const abandonedHolds = new Map<
    string,
    { readonly topic: string; readonly id: string; readonly lastSeen?: number }
  >();
  // Per session, a role the listener refused to that session's automatic claim because another
  // live session holds it: that holder is the newer one, so no pass claims the role for that
  // session again until a session event lands on it or the process restarts.
  const refusedRoles = new Map<string, string>();
  // The role a plugin last opened role-bound notices for: a pass that claims that role back after
  // a role end closed them says they stay closed until that plugin claims it again.
  let roleNoticesFor: string | undefined;
  // Notice subjects this session takes only while it holds `claimedRoleTopic`. They are never
  // registered with the listener, so a resumed process cannot recover them: the role's claim is
  // their only source, and a role end closes them.
  const roleNoticeSubjects = new Set<string>();
  // Counts role ends, so a role-bound subscription still opening when its role ended can tell.
  let roleEnds = 0;
  // When the listener last accepted a registration, and the session it was accepted for. A
  // registration accepted for an earlier session says nothing about the one running now, so it
  // reads as no registration at all; a session that never had one reads as older than any age.
  let registeredFor = "";
  let registeredAt = -Infinity;
  // The session whose agent subject, registration and Dispatch binding the last pass converged to.
  let establishedSession = "";
  // The session whose role the last pass decided.
  let roleSettledFor = "";
  // The sessions whose agent subject this process may have open.
  const agentSessions = new Set<string>();
  // The session whose agent subject, stream watch and registered interests a pass opened in full.
  let inboundOpenFor = "";

  const heldRole = (): string | undefined => claimedRoleTopic?.slice(ROLE_TOPIC_PREFIX.length);
  const isRoleBound = (subject: string): boolean => roleNoticeSubjects.has(subject);

  // The session stops holding its role: close the notice subjects it took for it, so a holder
  // another session replaced stops taking the role's wakes.
  const endRole = (): void => {
    claimedRoleTopic = undefined;
    claimedRoleSessionID = undefined;
    roleEnds += 1;
    for (const subject of roleNoticeSubjects) deps.closeSubject(subject);
    roleNoticeSubjects.clear();
  };

  const handBackAt = async (topic: string, sessionID: string): Promise<void> => {
    await deps.client().unsubscribe({ sessionID, topics: [topic] });
  };

  const refreshRegistration = (input: RegistrationInput): Promise<Interest> => {
    for (const [key, hold] of abandonedHolds) {
      if (hold.id !== input.sessionID) continue;
      abandonedHolds.delete(key);
      pendingHandBacks.set(key, { topic: hold.topic, id: input.sessionID });
    }
    return deps
      .client()
      .subscribe({ ...input, clientInstanceID })
      .then((result) => {
        registeredFor = input.sessionID;
        registeredAt = Date.now();
        return result;
      });
  };

  const recordClaimAfterDeadline = (topic: string, sessionID: string): void => {
    pendingHandBacks.set(`${topic}@${sessionID}`, { topic, id: sessionID });
  };

  const adoptClaimAfterSessionMoved = async (
    topic: string,
    sessionID: string,
    previousTopic: string | undefined,
    previousHoldID: string | undefined
  ): Promise<void> => {
    const moving = previousTopic !== undefined && previousTopic !== topic;
    if (moving) endRole();
    claimedRoleTopic = topic;
    claimedRoleSessionID = sessionID;
    if (moving && previousTopic !== undefined && previousHoldID) {
      await handBackAt(previousTopic, previousHoldID);
    }
  };

  /**
   * Reads the listener's `last_seen` for the session a refused claim of `role` names when that
   * session holds this process's own abandoned hold of `topic`, and answers whether the claim
   * waits on it. A read that fails, or finds another holder, leaves the next pass to decide, and
   * so does one the task's deadline outran: both answer true, the answer that records no refusal.
   */
  const observeAbandonedHold = async (
    topic: string,
    role: string,
    holder: string | undefined,
    live: () => boolean
  ): Promise<boolean> => {
    const key = `${topic}@${holder}`;
    const abandoned = abandonedHolds.get(key);
    if (abandoned === undefined) return false;
    const seen = await deps
      .client()
      .getRole(role)
      .then(
        (info) => (info.holder === holder ? info.last_seen : undefined),
        () => undefined
      );
    if (!live() || seen === undefined) return true;
    // The bound on reading another process's refusal as this process's own stale hold: a later
    // read that differs drops the hold, so the misreading costs at most one extra soft claim.
    if (abandoned.lastSeen !== undefined && seen !== abandoned.lastSeen) {
      abandonedHolds.delete(key);
      return false;
    }
    abandonedHolds.set(key, { topic, id: abandoned.id, lastSeen: seen });
    return true;
  };

  const createHandle = (live: () => boolean): TaskHandle => {
    let bound: string | undefined;
    // Whether this task may still write: its deadline has not fired and, once bound, its session
    // is still the open one. The one question every step below asks, asked here alone.
    const acting = (): boolean => live() && (bound === undefined || deps.sessionOpen(bound));
    const guard = (): void => {
      if (acting()) return;
      throw new StepRefused(
        live()
          ? `the task acting for session ${bound} refused a step: that session is no longer the open one`
          : "the task refused a step: it was abandoned at its deadline"
      );
    };
    const actingFor = (): string => {
      if (bound === undefined) {
        throw new Error("this queued task has not named the session it acts for (actFor)");
      }
      return bound;
    };

    const handBackHeldRole = async (tellUser: boolean): Promise<RoleEnded | undefined> => {
      guard();
      const role = heldRole();
      const holdID = claimedRoleSessionID;
      const topic = claimedRoleTopic;
      if (role === undefined || topic === undefined) return undefined;
      const pluginHeld = roleNoticeSubjects.size > 0;
      endRole();
      if (holdID !== undefined && holdID !== "") {
        await handBackAt(topic, holdID).catch((error: unknown) =>
          deps.log.warn("envoy: a role this process gave up was not handed back", {
            role,
            sessionID: holdID,
            error: messageFor(error),
          })
        );
      }
      // The end has landed, so the pane hears about it whatever became of this task.
      if (tellUser) {
        const host = bound ?? "";
        deps.log.warn("envoy: role ended after a move with no session event", {
          role,
          sessionID: host,
        });
        deps.notify(
          pluginHeld
            ? `envoy: role ${role} ended: the host moved to session ${host} with no session event, and that session's records do not name it; the plugin that opened its notices claims it again on its next session switch or restart`
            : `envoy: role ${role} ended: the host moved to session ${host} with no session event, and that session's records do not name it; if this session should hold it, run envoy_role_set`,
          "warning"
        );
      }
      return { role, pluginHeld };
    };

    const settleEndedHolds = async (wanted: string | undefined): Promise<boolean> => {
      guard();
      let restored = false;
      const failed: { readonly key: string; readonly topic: string; readonly id: string }[] = [];
      for (const [key, hold] of pendingHandBacks) {
        // Refused mid-hand-back: the task that replaces this one settles what is still pending, so
        // stop before the delete — a `Map` iterator visits entries inserted after it was created,
        // and an entry a session event added behind this loop belongs to that task. The holds whose
        // hand-back already failed are abandoned below: they are in neither map while this runs.
        if (!acting()) break;
        pendingHandBacks.delete(key);
        // A hold is adopted only for the session this process still registers: one under a session
        // it has left may be another process's now, and a claim naming it would take that live
        // process's role.
        if (
          wanted !== undefined &&
          hold.topic === ROLE_TOPIC_PREFIX + wanted &&
          deps.sessionOpen(hold.id)
        ) {
          claimedRoleTopic = hold.topic;
          claimedRoleSessionID = hold.id;
          holdUnconfirmed = true;
          restored = true;
          continue;
        }
        await handBackAt(hold.topic, hold.id).catch((error: unknown) => {
          failed.push({ key, ...hold });
          deps.log.warn("envoy: a role this process gave up was not handed back", {
            topic: hold.topic,
            sessionID: hold.id,
            error: messageFor(error),
          });
        });
      }
      for (const { key, topic, id } of failed) abandonedHolds.set(key, { topic, id });
      // Refused during the last hand-back, or at the break above: the restore this task made is the
      // replacing one's to decide.
      guard();
      return restored;
    };

    const claimRole = async (
      role: string,
      options: { readonly soft?: boolean; readonly previousSessionID?: string } = {}
    ): Promise<boolean> => {
      const id = actingFor();
      guard();
      const topic = ROLE_TOPIC_PREFIX + role;
      const previousTopic = claimedRoleTopic;
      const previousHoldID = claimedRoleSessionID;
      const result = await deps.client().setRole({
        sessionID: id,
        role,
        soft: options.soft,
        previousSessionID: options.previousSessionID,
      });
      if (!live()) {
        // The listener may have granted the claim while this task was abandoned. Nothing of it goes
        // in the transcript — the task that replaced this one owns the role from here — but the
        // hold is real at the listener, so leave it pending for the next pass to hand back.
        if (result.claimed) recordClaimAfterDeadline(topic, id);
        throw new StepRefused(
          `the role move that claimed ${role} for session ${id} was abandoned at its deadline while the claim was in flight; ${result.claimed ? "the hold the listener granted is left for the next pass to hand back" : "nothing was claimed"}`
        );
      }
      if (result.claimed) {
        refusedRoles.delete(id);
        holdUnconfirmed = false;
        // The listener names this claim's session for the role now, so no hold of it this process
        // gave up is left to hand back or wait on (one under this same id would end the new hold).
        for (const [key, hold] of abandonedHolds) {
          if (hold.topic === topic) abandonedHolds.delete(key);
        }
        for (const [key, hold] of pendingHandBacks) {
          if (hold.topic === topic) pendingHandBacks.delete(key);
        }
      }
      if (!deps.sessionOpen(id)) {
        // The session moved while the claim was in flight. A hold that landed is kept as this
        // process's own, under the id the listener names, and the pass the move queues converges
        // it. Handing it back here would lose a role a fork should carry.
        if (result.claimed) {
          await adoptClaimAfterSessionMoved(topic, id, previousTopic, previousHoldID);
        }
        throw new StepRefused(
          `the session moved on while its claim of role ${role} was in flight; ${result.claimed ? "the next pass keeps the hold for the session the host is on, or hands it back" : "nothing was claimed"}`
        );
      }
      if (!result.claimed) {
        endRole();
        // Only a soft claim is refused. A holder that is this process's own abandoned hold is
        // waited out, since the listener reaps it; any other is the newer holder.
        const waitingOnReap = await observeAbandonedHold(topic, role, result.holder, live);
        // The hold was read across a wait: this task may have been refused since, and the claim
        // that replaced it may already have cleared the refusal this one is about to record.
        // Nothing is written then, and the line says so.
        const notRecorded = !live()
          ? "the move was abandoned at its deadline before the refusal was recorded"
          : deps.sessionOpen(id)
            ? undefined
            : "the session moved on before the refusal was recorded";
        if (notRecorded === undefined && !waitingOnReap) refusedRoles.set(id, role);
        deps.log.warn(
          notRecorded !== undefined
            ? `envoy: role held by another live session; ${notRecorded}`
            : waitingOnReap
              ? "envoy: role still held by a session this process left; claiming again once the listener reaps it"
              : "envoy: role held by another live session; not reclaimed",
          { role, sessionID: id, holder: result.holder }
        );
        return false;
      }
      // Moving to another role ends the one this session held.
      const moving = previousTopic !== undefined && previousTopic !== topic;
      if (moving) endRole();
      claimedRoleTopic = topic;
      claimedRoleSessionID = id;
      // The transcript is the one thing `omp --resume` guarantees, so it is the durable record of
      // the claim: the listener releases a role claim one session TTL after heartbeats stop, and
      // this process's memory dies with it.
      deps.appendRoleClaimEntry(role);
      // The role it moved off is handed back under the id its hold was under (a fork's parent,
      // until a pass carried it).
      if (moving && previousTopic !== undefined && previousHoldID) {
        await handBackAt(previousTopic, previousHoldID);
      }
      return true;
    };

    const reassertRole = async (role: string, afterOutage: boolean): Promise<RoleReassertion> => {
      const id = actingFor();
      guard();
      const topic = ROLE_TOPIC_PREFIX + role;
      const client = deps.client();
      // Whether this process still holds the role the re-assertion was decided for: a session
      // event can end it while the listener is read or claimed, and the task that ends it owns
      // the role from there.
      const stillHolds = (): boolean => claimedRoleTopic === topic;
      const holder = await client.getRole(role).then(
        (info) => info.holder,
        (error: unknown) => {
          // 404 is the listener's answer for "no live holder", not a failure.
          if (error instanceof EnvoyApiError && error.details.status === 404) return undefined;
          throw error;
        }
      );
      guard();
      if (!stillHolds()) return { decided: false };
      if (holder === id) {
        return afterOutage ? { decided: true, regain: "reregistered" } : { decided: true };
      }
      const result = await client.setRole({ sessionID: id, role, soft: true });
      guard();
      if (!stillHolds()) return { decided: false };
      if (!result.claimed) {
        endRole();
        refusedRoles.set(id, role);
        deps.log.warn("envoy: role re-assertion refused; held by another live session", {
          role,
          sessionID: id,
          holder: result.holder,
        });
        deps.notify(
          `envoy: role ${role} is now held by session ${result.holder}; this session no longer holds it`,
          "warning"
        );
        return { decided: true };
      }
      deps.log.warn("envoy: role re-asserted after the listener lost the claim", {
        role,
        sessionID: id,
        previousHolder: holder ?? null,
      });
      return { decided: true, regain: "reclaimed" };
    };

    return {
      actFor: (id) => {
        if (bound !== undefined) {
          throw new Error(
            `this queued task already acts for session ${bound} and cannot act for ${id}`
          );
        }
        bound = id;
      },
      check: guard,
      register: (input) => {
        guard();
        // Timed whatever becomes of this task: the heartbeat's stall test reads one clock, and a
        // registration the listener accepted moved the session's `last_seen` whoever asked for it.
        return refreshRegistration(input);
      },
      markEstablished: (id) => {
        guard();
        establishedSession = id;
      },
      forgetOtherAgentSessions: (host) => {
        guard();
        const closed: string[] = [];
        for (const other of agentSessions) {
          if (other === host) continue;
          closed.push(other);
          agentSessions.delete(other);
          if (inboundOpenFor === other) inboundOpenFor = "";
        }
        return closed;
      },
      markAgentSessionOpen: (id) => {
        guard();
        agentSessions.add(id);
      },
      markInboundOpen: (id) => {
        guard();
        inboundOpenFor = id;
      },
      markRoleSettled: (id) => {
        guard();
        roleSettledFor = id;
      },
      handBackHeldRole,
      settleEndedHolds,
      claimRole,
      reassertRole,
      noteNoticesClosed: (role) => {
        guard();
        if (roleNoticesFor !== role || roleNoticeSubjects.size > 0) return;
        roleNoticesFor = undefined;
        deps.notify(
          `envoy: role ${role} is held again, but the notices a plugin opened for it stay closed until that plugin claims it again on its next session switch or restart`,
          "warning"
        );
      },
      readRoleHolder: async (role) => {
        guard();
        return deps
          .client()
          .getRole(role)
          .then(
            (info) => info.holder,
            () => undefined
          );
      },
      readInterest: async (sessionID) => {
        guard();
        return deps
          .client()
          .getInterest(sessionID)
          .catch(() => undefined);
      },
      handBack: async (topic, sessionID) => {
        guard();
        await handBackAt(topic, sessionID);
      },
      dial: async () => {
        guard();
        const dialled = await deps.dial();
        // The loser of the race is this dial: a task refused while it was in flight must not put
        // its connection anywhere, so it is closed here rather than left dangling.
        if (!acting()) {
          await dialled.close().catch(() => {});
          throw new StepRefused(
            live()
              ? `the task acting for session ${bound} dialled NATS for a session that is no longer the open one; the connection it opened was closed`
              : "the task that dialled NATS was abandoned at its deadline while the dial was in flight; the connection it opened was closed"
          );
        }
        return dialled;
      },
      subscribe: async (topic) => {
        guard();
        const opened = await deps.subscribeTopic(topic);
        guard();
        return opened;
      },
    };
  };

  const createQueue = (): TaskQueue => {
    let tail: Promise<unknown> = Promise.resolve();
    return {
      run<T>(task: (handle: TaskHandle) => Promise<T>): Promise<T | undefined> {
        // `tail` never rejects (it is always `next.catch(…)`), so a failed task never stops the
        // next.
        const next = tail.then(
          () =>
            new Promise<T | undefined>((resolve, reject) => {
              let abandoned = false;
              const taskDeadlineMs = deps.deadlineMs();
              const timer = setTimeout(() => {
                abandoned = true;
                reject(
                  new AbandonedTaskError(
                    `a queued task did not settle within ${taskDeadlineMs}ms and was abandoned; the queue moved on`
                  )
                );
              }, taskDeadlineMs);
              const handle = createHandle(() => !abandoned);
              // The task runs inside this promise so a synchronous throw is its rejection too.
              void (async () => task(handle))().then(
                (value) => {
                  // Clear first: a healthy task must not leave a pending timer holding the process
                  // open for a whole deadline.
                  clearTimeout(timer);
                  if (!abandoned) resolve(value);
                },
                (error: unknown) => {
                  clearTimeout(timer);
                  // An abandoned task's settlement is swallowed: the caller's promise rejected at
                  // the deadline, and an unhandled rejection is fatal to the host.
                  if (abandoned) return;
                  // The task's session moved: it decided nothing, and the pass or move the move
                  // queued decides. That is not a failure to report.
                  if (error instanceof StepRefused) resolve(undefined);
                  else reject(error);
                }
              );
            })
        );
        tail = next.catch(() => undefined);
        return next;
      },
      settled: (): Promise<unknown> => tail,
    };
  };

  return {
    clientInstanceID,
    passes: createQueue(),
    roleMoves: createQueue(),

    refreshRegistration,
    registrationFresherThan: (sessionID, ms) =>
      registeredFor === sessionID && Date.now() - registeredAt < ms,
    deregister: async (sessionID) => {
      await deps.client().unregisterSession({ sessionID, clientInstanceID });
    },
    clearRefusal: (sessionID) => {
      refusedRoles.delete(sessionID);
    },
    endUnwantedRole: (host, recordedRole, lineage) => {
      const held = heldRole();
      const topic = claimedRoleTopic;
      const holdID = claimedRoleSessionID;
      if (held === undefined || topic === undefined || holdID === undefined) return;
      const carried = holdID === host || lineage.includes(holdID);
      const wanted = recordedRole === undefined ? carried : recordedRole === held;
      if (wanted) return;
      endRole();
      if (holdID !== "") pendingHandBacks.set(`${topic}@${holdID}`, { topic, id: holdID });
    },
    markRoleBoundSubjects: (subjects, role) => {
      const marked: string[] = [];
      for (const subject of subjects) {
        if (roleNoticeSubjects.has(subject)) continue;
        roleNoticeSubjects.add(subject);
        marked.push(subject);
      }
      roleNoticesFor = role;
      return marked;
    },
    unmarkRoleBoundSubjects: (subjects) => {
      for (const subject of subjects) roleNoticeSubjects.delete(subject);
    },
    recordClaimAfterDeadline,
    adoptClaimAfterSessionMoved,

    heldRole,
    holdsRoleTopic: (topic) => claimedRoleTopic === topic,
    holdSessionID: () => claimedRoleSessionID,
    isRoleBound,
    roleBoundSubjects: () => [...roleNoticeSubjects],
    roleSnapshot: (topic) => ({
      roleBound: expandSubscriptionTopics([topic]).some(isRoleBound),
      endsBefore: roleEnds,
    }),
    refusalFor: (sessionID) => refusedRoles.get(sessionID),
    roleEndCount: () => roleEnds,
    holdUnconfirmed: () => holdUnconfirmed,
    establishedSession: () => establishedSession,
    inboundOpenFor: () => inboundOpenFor,
    roleSettledFor: () => roleSettledFor,
  };
}

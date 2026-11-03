/**
 * The Dispatch API's closed lists, caps and wire constants: the values the server enforces, the
 * clients refuse on, and `scripts/gen-go.ts` emits into Go, so each is one literal rather than a
 * server string and a client pattern that can drift apart. It imports nothing, so every other
 * module of the contract - and `dispatch-tools.ts`, whose tool schemas take the same caps - can
 * read it.
 */

/** Maximum entries in an issue's blocked_by input, before duplicate keys are removed. */
export const MAX_ISSUE_BLOCKERS = 20;

/**
 * The longest issue title, in UTF-16 units, counted after the trim `POST /api/v1/issues` and
 * `PATCH /api/v1/issues/{key}` apply; a longer one is `400 CAP_EXCEEDED`. Production's longest
 * title was 326 characters when this was set. It bounds the duplicate-title check, which compares
 * the new title with every title's lexemes in the project on each creation, and the
 * `POSSIBLE_DUPLICATE` message, which quotes a candidate's whole title. Generated into Go as
 * `contracts.IssueTitleMax`, which the server enforces, and the dashboard's title fields take it as
 * their `maxLength`, so the two agree.
 */
export const ISSUE_TITLE_MAX = 1000;

/**
 * The longest project name, in UTF-16 units, counted after the trim `POST /api/v1/projects`
 * applies; a longer one is `400 CAP_EXCEEDED`. A name shows beside its key in the sidebar's
 * 20rem rail and in every project picker's `KEY · Name` option, any bearer may write one, and no
 * route renames a project, so it is held to about two lines of that rail; production's longest name
 * was 25 characters when this was set. Generated into Go as `contracts.ProjectNameMax`, which the
 * server enforces, and the Settings name field and `dispatch project` take it as their maximum.
 */
export const PROJECT_NAME_MAX = 80;

/**
 * The most issues one page of `GET /api/v1/issues` holds: `limit` is 1 to this. Generated into Go
 * as `contracts.MaxIssuePageLimit`, which the server enforces, and the `dispatch_issues` tool's
 * `limit` takes the same bound, so the two ends agree.
 */
export const MAX_ISSUE_PAGE_LIMIT = 250;

/**
 * The page size of `GET /api/v1/issues` when a caller pages with `offset` alone, and of
 * `dispatch_issues` when it names no `limit`. Generated into Go as `contracts.DefaultIssuePageLimit`.
 */
export const DEFAULT_ISSUE_PAGE_LIMIT = 50;

/**
 * How many of an issue's events `dispatch_read` lists, the newest ones, when it names neither
 * `events` nor `all_events`: what `DispatchClient.read` returns by default.
 */
export const DEFAULT_ISSUE_READ_EVENTS = 10;

/** Who holds the turn on an open ask after a reply: `human` when the human needs to act,
 *  `agent` when the asking agent still owes the next move (a progress note). */
export const ASK_TURNS = ["human", "agent"] as const;
export type AskTurn = (typeof ASK_TURNS)[number];

/**
 * The closed list of targeted-delivery modes a session can advertise as
 * capabilities on the Envoy listener (`/v1/interests/subscribe`) and that
 * Dispatch can request for a message. Every other spelling of this set —
 * `MessageDeliveryMode`, the delivery event payload schema, the envoy-client
 * targeted-frame schema — derives from this constant.
 */
export const DELIVERY_CAPABILITIES = ["aside", "btw", "steer"] as const;

export type DeliveryCapability = (typeof DELIVERY_CAPABILITIES)[number];

export type MessageDeliveryMode = DeliveryCapability;

/**
 * The most sessions one broadcast sends to (`POST /api/v1/broadcasts`'s `session_ids`, after the
 * duplicates are dropped). A runaway guard, not a product limit: selection is a human ticking
 * boxes, but select-all makes a large send one click. Generated into Go as
 * `contracts.MaxBroadcastRecipients`, which the server enforces, so the dashboard's refusal and
 * the server's are one number.
 */
export const MAX_BROADCAST_RECIPIENTS = 100;

/**
 * The request header that names one write's intent: the same on every retry of that write, and
 * different for every new press. Dispatch accepts it, optionally, on every `POST` and `PATCH`
 * route (`PUT` and `DELETE` are idempotent already): a repeat with the same key and the same
 * request, once the first has finished, is answered with the first's stored status and body and
 * `IDEMPOTENCY_REPLAYED_HEADER`, and runs nothing again; the same key with a different request is
 * `422 IDEMPOTENCY_KEY_REUSED`; the same key while the first is still running (within
 * `IDEMPOTENCY_LEASE_MS`) is `409 IDEMPOTENCY_KEY_IN_FLIGHT` with `Retry-After: 1`, which a client
 * asks again after. A 5xx, 401, 408 or 429 is not stored, so the next attempt runs the request
 * again - unless the first attempt's write committed before its answer was lost, which a repeat
 * is answered `409 IDEMPOTENCY_KEY_COMMITTED` for. Keys belong to the signed-in login that sent
 * them and are kept `IDEMPOTENCY_KEY_TTL_MS`.
 * Five routes answer a repeat from the record they made rather than the stored body: `POST
 * /api/v1/agents/{session_id}/messages`, `POST /api/v1/broadcasts`, `POST
 * /api/v1/issues/{key}/messages`, `POST /api/v1/issues/{key}/comments` and `POST
 * /api/v1/artifacts/{id}/comments` resume any delivery still owed and answer `201` with
 * `IDEMPOTENCY_REPLAYED_HEADER` and the resource read afresh in the first answer's shape (a
 * broadcast with the first answer's `excluded`), so a recipient whose attempt a live sender holds
 * can still read `pending`, and a repeat after a lost answer is that replay, never
 * `409 IDEMPOTENCY_KEY_COMMITTED`. A write without the header is run as it always was, with no
 * replay (the IETF `Idempotency-Key` draft; Google AIP-155).
 */
export const IDEMPOTENCY_KEY_HEADER = "Idempotency-Key";

/** The response header (`?1`, a structured-field boolean) on an answer Dispatch replayed from
 *  the first request under the key rather than ran again. */
export const IDEMPOTENCY_REPLAYED_HEADER = "Idempotency-Replayed";

/** The longest `IDEMPOTENCY_KEY_HEADER` value, in characters. */
export const IDEMPOTENCY_KEY_MAX = 128;

/** A whole `IDEMPOTENCY_KEY_HEADER` value: letters, digits, `.`, `_`, `:` and `-`, from one to
 *  `IDEMPOTENCY_KEY_MAX` characters. Any other is `400 KEY_INVALID` (`IDEMPOTENCY_KEY_INVALID`). */
export const IDEMPOTENCY_KEY_PATTERN = new RegExp(`^[A-Za-z0-9._:-]{1,${IDEMPOTENCY_KEY_MAX}}$`);

/** The code of the `422` for a key already used for a different request (another method,
 *  route, path value or body); that request was not run. */
export const IDEMPOTENCY_KEY_REUSED = "KEY_REUSED";

/** The code of the `409` (with `Retry-After: 1`) for a key whose first request is still running;
 *  nothing was run, and the client asks again under the same key. */
export const IDEMPOTENCY_KEY_IN_FLIGHT = "KEY_IN_FLIGHT";

/** The code of the `409` for a key whose write landed before its answer was lost: Dispatch will
 *  not run it again and has no answer to replay. The write is done; a client treats it as landed
 *  and reads again what it wrote (the event feed carries the change too). */
export const IDEMPOTENCY_KEY_COMMITTED = "KEY_COMMITTED";

/** The code of the `400` for an `IDEMPOTENCY_KEY_HEADER` that does not match
 *  `IDEMPOTENCY_KEY_PATTERN`, or is sent twice: named as `KEY_REUSED`, `KEY_IN_FLIGHT` and
 *  `KEY_COMMITTED` are. */
export const IDEMPOTENCY_KEY_INVALID = "KEY_INVALID";

/** The code a replay's body carries when the first answer was larger than Dispatch keeps (1 MiB):
 *  the replay has the first answer's status and this body instead of the first's. A 2xx with it
 *  means the write ran; the client reads the resource again rather than this body. */
export const IDEMPOTENCY_ANSWER_NOT_KEPT = "ANSWER_NOT_KEPT";

/**
 * The request header a page puts on every write it makes (`POST`, `PATCH`, `PUT`, `DELETE`): the
 * login of the person who pressed it. Dispatch answers `409 DISPATCH_LOGIN_CHANGED` before
 * anything runs, its key untouched, when the request's caller is another person or no person at
 * all, so a write one person pressed never runs under another's session - another tab signs in
 * in place, and this tab's retry goes out on the new cookie before it learns of it. A caller that
 * resolves to nobody (a lapsed cookie) is answered by the route's own `401`. A write without the
 * header (the `dispatch` command, an agent) is not checked.
 */
export const DISPATCH_LOGIN_HEADER = "Dispatch-Login";

/** The code of the `409` for a write whose `DISPATCH_LOGIN_HEADER` names a login that is not its
 *  caller's: nothing ran. The page drops every write that login pressed. */
export const DISPATCH_LOGIN_CHANGED = "LOGIN_CHANGED";

/** The codes of a write Dispatch made and has no answer to give for: `409 KEY_COMMITTED`, or a
 *  replayed 2xx reading `ANSWER_NOT_KEPT`. A client treats either as landed. */
export type IdempotencyLandedCode =
  | typeof IDEMPOTENCY_KEY_COMMITTED
  | typeof IDEMPOTENCY_ANSWER_NOT_KEPT;

/**
 * Whether an attempt Dispatch answered with `status` (and `code`, its error body's) ended without
 * Dispatch's verdict on the key, so a client asks again under the same key: a 5xx, a 408 or a 429
 * (none of which Dispatch stores, `IDEMPOTENCY_KEY_HEADER`), or `409 IDEMPOTENCY_KEY_IN_FLIGHT`
 * (the first request under the key is still running). Any other answer is Dispatch's verdict, and
 * the same key would be answered with it again. The page and the `dispatch` command both retry by
 * this one rule.
 */
export function retriesUnderKey(status: number, code: string | undefined): boolean {
  return (
    status >= 500 ||
    status === 408 ||
    status === 429 ||
    (status === 409 && code === IDEMPOTENCY_KEY_IN_FLIGHT)
  );
}

/**
 * Whether an answer of `status`, `replayed` (it carried `IDEMPOTENCY_REPLAYED_HEADER`) and parsed
 * `body` is a replay whose first answer Dispatch did not keep: a replayed 2xx whose body is
 * `{ code: IDEMPOTENCY_ANSWER_NOT_KEPT }`. The write ran; the client reads the resource again
 * rather than taking this body for it. An answer that is not a replay is the resource itself,
 * whatever it holds.
 */
export function answerNotKept(status: number, replayed: boolean, body: unknown): boolean {
  return (
    replayed &&
    status >= 200 &&
    status < 300 &&
    typeof body === "object" &&
    body !== null &&
    Reflect.get(body, "code") === IDEMPOTENCY_ANSWER_NOT_KEPT
  );
}

/** How long Dispatch keeps a key's answer: 24 hours. */
export const IDEMPOTENCY_KEY_TTL_MS = 24 * 60 * 60 * 1000;

/** How long a key's first request holds the key: a repeat within this is
 *  `409 IDEMPOTENCY_KEY_IN_FLIGHT`. One after it, the first one's process presumed dead, is
 *  answered `409 IDEMPOTENCY_KEY_COMMITTED` when that first run's write committed, is answered
 *  from the record on the five routes that keep one (`IDEMPOTENCY_KEY_HEADER`), and otherwise
 *  takes the key over and runs the request again. 60 seconds. */
export const IDEMPOTENCY_LEASE_MS = 60 * 1000;

/**
 * How long a repeated delivery is recognised, in milliseconds: the window behind the dashboard's
 * promise that a same-mode Retry "won't deliver it twice". This comment is where that promise is
 * stated; every other place that relies on it points here.
 *
 * Dispatch keys a send by message and mode (`<message>:<mode>`, `<comment>:<target>:<mode>`), the
 * same for every attempt, and the listener makes that key, scoped to the recipient
 * (`agent.<session>.<key>`), the envelope's `dedupe_key`. A same-mode Retry of a send that already
 * landed therefore repeats that frame's dedupe key, and only that: the listener mints a new
 * `event_id` for every send. Two things recognise the repeat inside this window, both only for a
 * key that names its event (`dedupeKeyNamesItsEvent` in `envelope.ts`, which every Dispatch key
 * does), and a recipient is protected by whichever of them sits between it and the bus:
 *
 * - The notification stream stores such a frame under a MsgId of its dedupe key and topic, so it
 *   keeps one copy and answers the repeat as a duplicate, which the attempt records as
 *   `duplicate`. That protects a session the listener pushes to from the stream. It does not
 *   protect a session that subscribes over core NATS: JetStream's check applies to the stream's
 *   copy only, and a core subscriber is handed every publish, repeats included.
 * - Each host that subscribes over core NATS (the Oh My Pi extension, the Claude Code channel)
 *   hands its agent at most one frame per such key inside this window, through
 *   `createDeliveryDedupe` in `@legion/envoy-client/delivery`. A frame the host answered with an
 *   error instead (a BTW whose side turn failed) was never handed over, so the host releases its
 *   key and a same-mode re-send of it reaches the agent.
 *
 * It fails in three places. A host's `createDeliveryDedupe` holds its keys in memory, so a host
 * that keeps nothing else and restarts after the first frame landed has forgotten it, and a Retry
 * reaches that agent a second time; that is ordinary, not rare: `claude --resume` starts a new
 * channel process on the same conversation, and a Sandbox pod resumes its session in a new
 * process. pi-envoy and claude-envoy, from the releases that carry `@legion/envoy-client/replay`,
 * also keep a ledger of the frames their session was handed (pi-envoy in the session's
 * transcript, claude-envoy in a file per session id) and drop a repeat it holds after a restart,
 * for 72 hours and up to 20,000 frames; only a crash between the hand-over and the ledger write
 * still lets that Retry through. Older plugins still forget, and the dashboard cannot tell which
 * one a session runs. The stream stores a re-send of an attempt the session answered with an
 * error no more than any other repeat, so a session the listener pushes to never receives it, and
 * its attempt reads as a duplicate. The dashboard therefore offers no same-mode Retry for such an
 * attempt and points at a mode change, which is a new key; sending that Retry under a new key is
 * LEGION-431. And a host keeps at most `DELIVERY_DEDUPE_KEY_LIMIT` keys, forgetting the oldest
 * past it, so a re-send of a key it was handed before that many later ones reaches the agent
 * again. The limit is two and a half times what the whole stream stored in
 * one window when it was set, so a host reaches it only once traffic grows past that or a
 * producer floods a topic it follows.
 *
 * Past the window neither remembers the key, and a same-mode retry is a second delivery, so the
 * dashboard makes the promise only inside it, and names the restart
 * (`packages/dispatch/web/src/features/conversation/delivery.ts`). `scripts/gen-go.ts` emits this
 * as `contracts.DeliveryDuplicateWindow`, which `bus/stream.go` uses for both the stream's
 * duplicate window and its retention.
 *
 * It also bounds webhook redelivery dedupe: a GitHub, Slack or Ghost Wispr envelope is stored
 * under a MsgId of its delivery id, and GitHub redelivers deliveries up to three days old, so a
 * window shorter than that lets a GitHub redelivery be stored, and handed to a host, twice.
 *
 * This window bounds a *delivery* retry, not a repeated *write*: a write's `Idempotency-Key`
 * (`IDEMPOTENCY_KEY_HEADER`) is recognised for `IDEMPOTENCY_KEY_TTL_MS`, however late within
 * that the repeat arrives.
 */
export const DELIVERY_DUPLICATE_WINDOW_MS = 72 * 60 * 60 * 1000;

/**
 * The cause a delivery attempt records when the listener never answered its send. The listener
 * publishes before it answers, so this is the one failure that may already have reached the
 * recipient — which is what makes re-sending in another mode a genuine second delivery, and what
 * the dashboard keys that wording on. Generated into Go as `contracts.ReceiptTimeoutCause`, so
 * the string the server writes and the string the dashboard recognises are one literal.
 *
 * It is the CAUSE only: the advice that follows from it expires with
 * `DELIVERY_DUPLICATE_WINDOW_MS`, so it is composed at render time and never stored.
 */
export const RECEIPT_TIMEOUT_CAUSE =
  "The listener didn't answer within the send window; the message may already have been delivered.";

/** `SearchResponse.degraded`'s one value today: meaning search could not run for this request (no
 * embedder configured, the query's own embedding timed out, or the model service answered an
 * error) and search fell back to keyword-only ranking. Generated into Go as
 * `contracts.SearchDegradedEmbedderUnavailable` so the string Dispatch writes and the string
 * `search-answer.ts` compares against cannot drift apart. */
export const SEARCH_DEGRADED_EMBEDDER_UNAVAILABLE = "embedder_unavailable";

/** What a `SessionAnswer` is to the session Dispatch delivered it to, one snake_case wire value per
 *  relation, each spelling the phrase a reader prints: a person answered the session's ask,
 *  replied in its ask's thread, replied in a comment thread it wrote in, commented on a document it
 *  wrote, commented on an issue routed to it, mentioned it, sent it a message (direct, on an issue,
 *  or a broadcast's copy), or replied to its message. Mirrored in Go as the
 *  `model.SessionAnswerRelation*` constants. */
export const SESSION_ANSWER_RELATIONS = [
  "answered_your_ask",
  "replied_on_your_ask",
  "replied_in_a_comment_thread",
  "commented_on_a_document",
  "commented_on_an_issue",
  "mentioned_you",
  "sent_you_a_message",
  "replied_to_your_message",
] as const;

export type SessionAnswerRelation = (typeof SESSION_ANSWER_RELATIONS)[number];

/** The code of the `401` Dispatch itself answers a caller it cannot resolve to a person - no
 *  session cookie, one it cannot read, one whose generation it has revoked, or a membership the
 *  sign-in issuer no longer confirms (`identity.WriteError`). A `401` without it came from
 *  something in front of Dispatch, an authenticating proxy among them, and says nothing about
 *  the session: a sign-out answered that way did not go through. */
export const AUTH_NO_IDENTITY = "NO_IDENTITY";

/**
 * The close code the document websocket (`/ws/doc/{room}`) refuses a room with when the tree it
 * holds is outside the Proof schema: it completes the upgrade and closes with this code, and
 * `DOCUMENT_SCHEMA_CLOSE_REASON`, before any sync, so a browser editor never receives a tree it
 * would normalize and write back. It is in the private 4000-4999 range beside Hocuspocus's own 4401
 * and 4403, and the dashboard reads it as the document's repair state, never as a dropped
 * connection to retry. Generated into Go as `contracts.DocumentSchemaCloseCode`.
 */
export const DOCUMENT_SCHEMA_CLOSE_CODE = 4409;

/** The reason the document websocket closes with `DOCUMENT_SCHEMA_CLOSE_CODE`: the code reads of
 *  that document answer (`409 DOC_SCHEMA`). Generated into Go as
 *  `contracts.DocumentSchemaCloseReason`. */
export const DOCUMENT_SCHEMA_CLOSE_REASON = "DOC_SCHEMA";

/** The close code Dispatch ends a live socket with once the socket reaches its age, a random point
 *  in [LIVE_SOCKET_MIN_AGE_MS, LIVE_SOCKET_MAX_AGE_MS). It is in the private 4000-4999 range, apart
 *  from the 4000 a page closes a silent socket with: the page reopens at once and resumes every
 *  subscription, showing no pill and refreshing nothing. */
export const LIVE_SOCKET_MAX_AGE_CLOSE = 4001;
/** The youngest age at which Dispatch ends a live socket: 30 minutes. */
export const LIVE_SOCKET_MIN_AGE_MS = 1_800_000;
/** The age before which Dispatch ends every live socket: 60 minutes. */
export const LIVE_SOCKET_MAX_AGE_MS = 3_600_000;
/** The most subscriptions one live socket holds at once; a subscribe past it is refused with a
 *  `TOO_MANY_SUBSCRIPTIONS` error frame. */
export const LIVE_SOCKET_MAX_SUBSCRIPTIONS = 64;

/**
 * The subject Dispatch announces its server fleet's build on, directly on the bus rather than
 * through the outbox (it is no issue's, document's or project's event), as an Envoy envelope with
 * `source` `dispatch` whose payload is `DispatchServerStartedPayload`. One envelope each time the
 * commit every live Dispatch server process serves changes: a rolling update is announced once its
 * last process of the build before has gone, a restart on the announced build is not announced, and
 * a rollback to an earlier build is announced again. Its event id, source event id and dedupe key
 * are `dispatch-server-started-<commit>-<seq>`, `seq` being the announcement's own number. Mirrored
 * by `startup.Subject` in `packages/envoy/internal/dispatch/startup`, and matched whole by the
 * Legion daemon's intake.
 */
export const DISPATCH_SERVER_STARTED_SUBJECT = "notifications.dispatch.server.started";

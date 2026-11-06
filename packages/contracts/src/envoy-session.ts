// The timings of Envoy session registration, declared here once and read by every language that
// holds one of them: the pi-envoy plugin that refreshes a pane's listener registration, the
// envoy-client transport that makes the call, the Go listener that judges a session live by its
// last registration, and the daemon that times the copies of a notice that listener could not
// forward.
//
// The numbers are one arrangement, not six independent settings: three refresh intervals plus one
// whole listener call - SESSION_HEARTBEAT_MS * 3 plus LISTENER_CALL_ATTEMPTS * timeout +
// (LISTENER_CALL_ATTEMPTS - 1) * retryDelay, which is 10,250 ms - must fall strictly short of
// SESSION_LIVE_WINDOW_MS: 3 * 90,000 + 10,250 = 280,250 ms inside 300,000 ms. A session that stops
// reading live unless it is refreshed is refreshed at under a third of its live window, so two
// whole refreshes may fail and the third still saves it; and that third refresh must COMPLETE, not
// merely be posted, before the window closes. At equality it has not yet paid for its round trip,
// and `setInterval` only ever fires late, so equality really buys one failed refresh, not two.
// Change one of these numbers and check the sum again.

/**
 * The interval at which an Oh My Pi pane's pi-envoy plugin refreshes its Envoy listener
 * registration. The Envoy listener reads a session as unresponsive `SESSION_LIVE_WINDOW_MS` after
 * its last registration, so this interval is set for three of them plus one whole call's budget to
 * fall strictly short of that window: two whole refreshes may fail and the third still completes
 * before the session stops reading live. Generated into Go as `contracts.SessionHeartbeat` so the
 * pane's refresh and the listener's live window are one arrangement rather than two numbers that
 * drift apart.
 */
export const SESSION_HEARTBEAT_MS = 90_000;

/**
 * How long a session's last registration keeps it live to the Envoy listener: `session.LiveWindow`
 * (`packages/envoy/internal/session/state.go`), the window every reader of the session registry
 * judges a record's age against. Past it the session reads unresponsive to every list, lookup and
 * send that asks for live sessions alone. Generated into Go as `contracts.SessionLiveWindow` so
 * the window the listener judges a registration by and the window the heartbeat interval is
 * derived from are one number.
 */
export const SESSION_LIVE_WINDOW_MS = 300_000;

/**
 * How long a session's role claim may go unrefreshed before a reader treats the process that
 * wrote it as gone. At `SESSION_HEARTBEAT_MS` this allows three missed heartbeats before a reader
 * assumes the holder is gone. The Legion daemon reads it to time the last copy of a notice the
 * listener could not forward (`noticeReholdDelays`, `packages/daemon/internal/daemon`). No role
 * resolution weighs it: a holder is judged by how long ago its registration was written, and a
 * session route's own write weighs no claim's freshness, since the route follows whichever process
 * heartbeated last. Generated into Go as `contracts.SessionClaimStale`, which the listener's
 * `session.ClaimStaleAfter` is, so the listener's window and the daemon's copy are one number.
 */
export const SESSION_CLAIM_STALE_MS = 300_000;

/**
 * How long one attempt of an envoy-client call waits for the listener's answer before it is
 * abandoned and, if attempts remain, retried. Generated into Go as `contracts.ListenerCallTimeout`
 * so the budget a whole call is allowed and the live window a refresh must complete inside are
 * weighed against the same attempt cost.
 */
export const LISTENER_CALL_TIMEOUT_MS = 5_000;

/**
 * The delay between one envoy-client attempt and the next: long enough for a listener restarting
 * under the same address to be listening again, short enough to leave the whole call inside its
 * caller's budget. Generated into Go as `contracts.ListenerCallRetryDelay` so a whole call's cost
 * is computed from one number on both sides.
 */
export const LISTENER_CALL_RETRY_DELAY_MS = 250;

/**
 * How many attempts one envoy-client call makes, unless the caller turns retrying off, in which
 * case it makes one. Generated into Go as `contracts.ListenerCallAttempts` so a whole call's cost
 * - attempts * `LISTENER_CALL_TIMEOUT_MS` plus the delays between them - is the same number
 * wherever it is checked against `SESSION_LIVE_WINDOW_MS`.
 */
export const LISTENER_CALL_ATTEMPTS = 2;

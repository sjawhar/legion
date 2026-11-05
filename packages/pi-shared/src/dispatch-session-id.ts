// packages/pi-shared/src/dispatch-session-id.ts
//
// The session an Oh My Pi process is acting as now, for every plugin of that process.

/**
 * The session this process is acting as now: the id pi-envoy's plugin last bound
 * (`bindDispatchSession` in packages/pi-envoy/extensions/envoy.ts, which writes
 * `DISPATCH_SESSION_ID` for the session's shell commands too), and "" while it has bound none.
 *
 * Every caller that trades at the secrets broker passes this rather than a name fixed when a
 * configuration resolved, because one Oh My Pi process moves between sessions over its life and
 * a token names one session: the listener answers 403 to a session presenting another's. Both
 * plugins of the process read the one variable, so a Legion read presents the session making it
 * rather than the one the process started on.
 */
export function currentSessionID(): string {
  return process.env.DISPATCH_SESSION_ID ?? "";
}

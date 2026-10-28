import { envoyPluginInterface, readEnvoyPluginInterface } from "./interface";

// The startup-turn mark (`EnvoyPluginInterface.startupTurn`) names a session whose first run has
// not started while its start is on the way: the start message the Legion entry sends to an idle
// session, or the start prompt the daemon delivers once its controller reports ready. The Legion
// entry marks it right before that start (`markStartupTurnSent`) and clears it from that session's
// `agent_start` and `session_shutdown` (`clearStartupTurn`). A start message queued as a follow-up
// behind a turn the operator already started is never marked: that run, the session's first, has
// already fired its one `agent_start`, and the follow-up runs inside it.

/** Marks `sessionID` as the session whose start was sent now. */
export function markStartupTurnSent(sessionID: string): void {
  envoyPluginInterface().startupTurn = { session: sessionID, sentAt: Date.now() };
}

/**
 * Clears the mark when it names `sessionID`. A `task` subagent's run has its own session id and
 * clears nothing; nor does any subagent's shutdown. Read only at this build's interface version: a
 * session that is not Legion's may run beside an Envoy entry speaking another.
 */
export function clearStartupTurn(sessionID: string): void {
  const reading = readEnvoyPluginInterface();
  if (reading.kind !== "present") return;
  if (reading.iface.startupTurn?.session !== sessionID) return;
  reading.iface.startupTurn = undefined;
}

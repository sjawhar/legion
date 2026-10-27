import { envoyPluginInterface, readEnvoyPluginInterface } from "./interface";

// The startup-turn mark (`EnvoyPluginInterface.startupTurn`) names a session until its first run
// starts: the start turn, or a turn the operator started first, which the start message then
// follows. The Legion entry marks it right before each start it sends (`markStartupTurnSent`) and
// clears it from that session's `agent_start` and `session_shutdown` (`clearStartupTurn`).

/** Marks `sessionID` as the session whose start was sent now. */
export function markStartupTurnSent(sessionID: string): void {
  const mark = envoyPluginInterface().startupTurn;
  mark.session = sessionID;
  mark.sentAt = Date.now();
}

/**
 * Clears the mark when it names `sessionID`. A `task` subagent's run has its own session id and
 * clears nothing; nor does any subagent's shutdown. Read only at this build's interface version: a
 * session that is not Legion's may run beside an Envoy entry speaking another.
 */
export function clearStartupTurn(sessionID: string): void {
  const reading = readEnvoyPluginInterface();
  if (reading.kind !== "present") return;
  const mark = reading.iface.startupTurn;
  if (mark.session !== sessionID) return;
  mark.session = undefined;
  mark.sentAt = undefined;
}

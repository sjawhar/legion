const subagentSessions = new Set<string>();

export function registerSubagentSession(sessionID: string): void {
  subagentSessions.add(sessionID);
}

export function unregisterSubagentSession(sessionID: string): void {
  subagentSessions.delete(sessionID);
}

export function isSubagentSession(sessionID: string): boolean {
  return subagentSessions.has(sessionID);
}

export function subagentQuestionBlockerHook(
  input: { tool: string; sessionID: string; callID: string },
  _output: { args: Record<string, unknown> }
): void {
  const toolName = input.tool?.toLowerCase();
  if (toolName !== "question" && toolName !== "askuserquestion") return;
  if (!subagentSessions.has(input.sessionID)) return;

  throw new Error(
    "Question tool is disabled for subagent sessions. " +
      "Subagents should complete their work autonomously without " +
      "asking questions. Return to the parent agent with your " +
      "findings and uncertainties."
  );
}

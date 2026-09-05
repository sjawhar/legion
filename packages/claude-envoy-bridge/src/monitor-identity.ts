export type MonitorEnvironment = {
  readonly ENVOY_SESSION_ID?: string | undefined
  readonly CLAUDE_CODE_SESSION_ID?: string | undefined
}

export function monitorSessionId(environment: MonitorEnvironment): string {
  const configured = environment.ENVOY_SESSION_ID
  if (configured && configured.trim().length > 0) {
    return configured
  }

  const claudeSession = environment.CLAUDE_CODE_SESSION_ID
  if (claudeSession && claudeSession.trim().length > 0) {
    return claudeSession
  }

  throw new Error(
    "Envoy monitor requires ENVOY_SESSION_ID or CLAUDE_CODE_SESSION_ID; Claude Code did not provide a session identity",
  )
}

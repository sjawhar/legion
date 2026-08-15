export type MonitorEnvironment = {
  readonly ENVOY_SESSION_ID?: string | undefined
  readonly CLAUDE_SESSION_ID?: string | undefined
}

export function monitorSessionId(environment: MonitorEnvironment, parentProcessId: number): string {
  const configured = environment.ENVOY_SESSION_ID
  if (configured && configured.trim().length > 0) {
    return configured
  }

  const claudeSession = environment.CLAUDE_SESSION_ID
  if (claudeSession && claudeSession.trim().length > 0) {
    return claudeSession
  }

  return `claude-${parentProcessId}`
}

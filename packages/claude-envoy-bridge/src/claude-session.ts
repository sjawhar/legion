export interface ClaudeSessionEnvironment {
  readonly ENVOY_SESSION_ID?: string | undefined
  readonly CLAUDE_CODE_SESSION_ID?: string | undefined
  readonly CLAUDE_PROJECT_DIR?: string | undefined
}

/** Resolve the session route that Claude Code supplies, allowing a QA override. */
export function claudeSessionId(environment: ClaudeSessionEnvironment): string {
  const configured = environment.ENVOY_SESSION_ID
  if (configured !== undefined && configured.trim().length > 0) return configured
  const claudeSession = environment.CLAUDE_CODE_SESSION_ID
  if (claudeSession !== undefined && claudeSession.trim().length > 0) return claudeSession
  throw new Error(
    "Envoy channel requires ENVOY_SESSION_ID or CLAUDE_CODE_SESSION_ID; Claude Code did not provide a session identity",
  )
}

/** The plugin process runs from its package root; Dispatch operations remain project-scoped. */
export function claudeProjectDirectory(
  environment: ClaudeSessionEnvironment,
  fallback: string,
): string {
  const projectDirectory = environment.CLAUDE_PROJECT_DIR
  return projectDirectory !== undefined && projectDirectory.trim().length > 0
    ? projectDirectory
    : fallback
}

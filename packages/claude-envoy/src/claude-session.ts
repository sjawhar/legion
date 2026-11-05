export interface ClaudeSessionEnvironment {
  readonly ENVOY_SESSION_ID?: string | undefined
  readonly CLAUDE_CODE_SESSION_ID?: string | undefined
  readonly CLAUDE_PROJECT_DIR?: string | undefined
}

/** An environment value that is set and not blank, untrimmed; undefined otherwise. */
export function configuredValue(value: string | undefined): string | undefined {
  return value !== undefined && value.trim().length > 0 ? value : undefined
}

/** The session route Claude Code supplies, allowing a QA override, or undefined where it
 *  supplies none. The one place that spells the precedence: a caller that must have an id calls
 *  `claudeSessionId`, and one that acts as a session only where its shell names one — a
 *  command's broker trade, which writes under that session — reads it here. */
export function configuredSessionId(environment: ClaudeSessionEnvironment): string | undefined {
  return (
    configuredValue(environment.ENVOY_SESSION_ID) ??
    configuredValue(environment.CLAUDE_CODE_SESSION_ID)
  )
}

/** Resolve the session route that Claude Code supplies, allowing a QA override. */
export function claudeSessionId(environment: ClaudeSessionEnvironment): string {
  const id = configuredSessionId(environment)
  if (id !== undefined) return id
  throw new Error(
    "Envoy channel requires ENVOY_SESSION_ID or CLAUDE_CODE_SESSION_ID; Claude Code did not provide a session identity",
  )
}

/** The plugin process runs from its package root; Dispatch operations remain project-scoped. */
export function claudeProjectDirectory(
  environment: ClaudeSessionEnvironment,
  fallback: string,
): string {
  return configuredValue(environment.CLAUDE_PROJECT_DIR) ?? fallback
}

export interface RuntimeStartOptions {
  workspace: string;
  logDir?: string;
  env?: Record<string, string>;
}

export interface RuntimeAdapter {
  /** Start the runtime (spawn serve process / create tmux session) */
  start(opts: RuntimeStartOptions): Promise<void>;

  /** Stop the runtime (kill serve / kill tmux session) */
  stop(): Promise<void>;

  /** Check if the runtime is healthy */
  healthy(): Promise<boolean>;

  /** Create a session for a worker or controller */
  createSession(sessionId: string, workspace: string): Promise<string>;

  /** Send a prompt to an existing session */
  sendPrompt(sessionId: string, text: string): Promise<void>;

  /** Get the port for direct client connections (0 if not applicable).
   * NOTE: This is an OpenCode-specific concept. ClaudeCode returns 0.
   * Consider removing if adapter-specific port exposure becomes problematic.
   */
  getPort(): number;
  /** Get the status of a session (returns opaque data forwarded to the client) */
  getSessionStatus(sessionId: string): Promise<{ data?: unknown; error?: unknown }>;
}

export interface BackgroundTask {
  id: string;
  status: "pending" | "running" | "completed" | "failed" | "cancelled";
  agent: string;
  model: string;
  description: string;
  sessionID?: string;
  parentSessionID?: string;
  /** Spawn depth: 0 for root tasks, parent.depth + 1 for children. */
  depth?: number;
  /** Session ID of the root ancestor task. Same as sessionID for root tasks. */
  rootSessionID?: string;
  result?: string;
  error?: string;
  createdAt: number;
  completedAt?: number;
  /** Maximum duration in ms before auto-cancellation. Undefined = no timeout. */
  timeoutMs?: number;
}

export interface LaunchOptions {
  agent: string;
  prompt: string;
  description: string;
  parentSessionId?: string;
  model?: string;
  systemPrompt?: string;
  /** Maximum duration in ms before auto-cancellation. Undefined = no timeout. */
  timeoutMs?: number;
}

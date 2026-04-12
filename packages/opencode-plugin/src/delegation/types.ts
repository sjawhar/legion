export interface BackgroundTask {
  id: string;
  status: "pending" | "running" | "completed" | "failed" | "cancelled";
  agent: string;
  model: string;
  description: string;
  sessionID?: string;
  parentSessionID?: string;
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

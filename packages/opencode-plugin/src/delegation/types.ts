export interface BackgroundTask {
  id: string;
  status: "pending" | "running" | "completed" | "failed" | "cancelled";
  agent: string;
  model: string;
  description: string;
  sessionID?: string;
  result?: string;
  error?: string;
  createdAt: number;
  completedAt?: number;
}

export interface LaunchOptions {
  agent: string;
  prompt: string;
  description: string;
  parentSessionId?: string;
  model?: string;
  skills?: string[];
  systemPrompt?: string;
}

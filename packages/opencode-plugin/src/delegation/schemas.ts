import { z } from "zod";

export const BackgroundTaskStatusSchema = z.enum([
  "pending",
  "running",
  "completed",
  "failed",
  "cancelled",
]);

export const BackgroundTaskSchema = z
  .object({
    id: z.string(),
    status: BackgroundTaskStatusSchema,
    agent: z.string(),
    model: z.string(),
    description: z.string(),
    sessionID: z.string().optional(),
    parentSessionID: z.string().optional(),
    result: z.string().optional(),
    error: z.string().optional(),
    createdAt: z.number(),
    completedAt: z.number().optional(),
    retryCount: z.number().optional(),
    concurrencyKey: z.string().optional(),
    lastMessageCount: z.number().optional(),
    lastActivityAt: z.number().optional(),
    staleAlertSent: z.boolean().optional(),
  })
  .passthrough();

import { z } from "zod";

export const TaskStatusSchema = z.enum(["pending", "in_progress", "completed", "cancelled"]);
export type TaskStatus = z.infer<typeof TaskStatusSchema>;

export const TaskSchema = z
  .object({
    id: z.string(),
    subject: z.string(),
    description: z.string(),
    status: TaskStatusSchema,
    blocks: z.array(z.string()).default([]),
    blockedBy: z.array(z.string()).default([]),
    owner: z.string().optional(),
    metadata: z.record(z.string(), z.unknown()).optional(),
    threadID: z.string(),
    parentID: z.string().optional(),
  })
  .strict();

export type Task = z.infer<typeof TaskSchema>;

export const TaskCreateInputSchema = z.object({
  subject: z.string(),
  description: z.string().optional(),
  blocks: z.array(z.string()).optional(),
  blockedBy: z.array(z.string()).optional(),
  owner: z.string().optional(),
  metadata: z.record(z.string(), z.unknown()).optional(),
  parentID: z.string().optional(),
});

export type TaskCreateInput = z.infer<typeof TaskCreateInputSchema>;

export const TaskGetInputSchema = z.object({
  id: z.string(),
});

export type TaskGetInput = z.infer<typeof TaskGetInputSchema>;

export const TaskUpdateInputSchema = z.object({
  id: z.string(),
  subject: z.string().optional(),
  description: z.string().optional(),
  status: TaskStatusSchema.optional(),
  addBlocks: z.array(z.string()).optional(),
  addBlockedBy: z.array(z.string()).optional(),
  owner: z.string().optional(),
  metadata: z.record(z.string(), z.unknown()).optional(),
  parentID: z.string().optional(),
});

export type TaskUpdateInput = z.infer<typeof TaskUpdateInputSchema>;

export const TaskListInputSchema = z.object({
  status: TaskStatusSchema.optional(),
  parentID: z.string().optional(),
  ready: z.boolean().optional(),
});

export type TaskListInput = z.infer<typeof TaskListInputSchema>;

/** Status values that satisfy a dependency (unblock downstream tasks). */
export const SATISFYING_STATUSES: ReadonlySet<TaskStatus> = new Set(["completed", "cancelled"]);

/** Maximum claim attempts before a task is escalated. */
export const MAX_CLAIM_ATTEMPTS = 3;

/** Default lease duration in milliseconds (5 minutes). */
export const LEASE_DURATION_MS = 5 * 60 * 1000;

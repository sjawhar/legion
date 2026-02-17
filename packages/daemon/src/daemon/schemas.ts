import { z } from "zod";

export const CrashHistoryEntrySchema = z.object({
  crashCount: z.number(),
  lastCrashAt: z.string().nullable(),
});

export const WorkerEntrySchema = z
  .object({
    id: z.string(),
    port: z.number(),
    sessionId: z.string(),
    workspace: z.string(),
    startedAt: z.string(),
    status: z.enum(["starting", "running", "stopped", "dead"]),
    crashCount: z.number(),
    lastCrashAt: z.string().nullable(),
  })
  .passthrough();

export const ControllerStateSchema = z.object({
  sessionId: z.string(),
  port: z.number().optional(),
  pid: z.number().optional(),
});

export const PersistedWorkerStateSchema = z
  .object({
    workers: z.record(z.string(), WorkerEntrySchema),
    crashHistory: z.record(z.string(), CrashHistoryEntrySchema),
    controller: ControllerStateSchema.optional(),
  })
  .passthrough();

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
export const LinearTeamsResponseSchema = z
  .object({
    data: z
      .object({
        teams: z
          .object({
            nodes: z.array(
              z
                .object({
                  id: z.string(),
                  key: z.string(),
                  name: z.string(),
                })
                .passthrough()
            ),
          })
          .passthrough(),
      })
      .nullable()
      .optional(),
    errors: z.array(z.object({ message: z.string() }).passthrough()).optional(),
  })
  .passthrough();

export type LinearTeamsResponse = z.infer<typeof LinearTeamsResponseSchema>;

export const SessionCreateResponseSchema = z
  .object({
    id: z.string(),
  })
  .passthrough();

export type SessionCreateResponse = z.infer<typeof SessionCreateResponseSchema>;

export const HealthCheckResponseSchema = z
  .object({
    healthy: z.boolean(),
  })
  .passthrough();

export const LegionEntrySchema = z.object({
  port: z.number(),
  servePort: z.number(),
  pid: z.number(),
  startedAt: z.string(),
});

export const LegionsRegistrySchema = z.record(z.string(), LegionEntrySchema);

export type HealthCheckResponse = z.infer<typeof HealthCheckResponseSchema>;

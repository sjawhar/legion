import { appendFile, rename, stat, unlink } from "node:fs/promises";

import { z } from "zod";

const FeedbackEventBase = z.object({
  schemaVersion: z.literal(1),
  timestamp: z.string(),
  legionId: z.string(),
  event: z.string(),
});

export const WorkerDispatchedEventSchema = FeedbackEventBase.extend({
  event: z.literal("worker.dispatched"),
  issueId: z.string(),
  mode: z.string(),
  workerId: z.string(),
  sessionId: z.string(),
  version: z.number().int().nonnegative(),
  workspace: z.string(),
  crashCount: z.number().int().nonnegative(),
});

export const WorkerStatusChangedEventSchema = FeedbackEventBase.extend({
  event: z.literal("worker.status_changed"),
  workerId: z.string(),
  issueId: z.string(),
  mode: z.string(),
  sessionId: z.string(),
  version: z.number().int().nonnegative(),
  fromStatus: z.string(),
  toStatus: z.string(),
  crashCount: z.number().int().nonnegative(),
  uptimeMs: z.number().nullable(),
});

export const StateCollectedEventSchema = FeedbackEventBase.extend({
  event: z.literal("state.collected"),
  issueId: z.string(),
  status: z.string(),
  suggestedAction: z.string(),
  hasLiveWorker: z.boolean(),
  workerMode: z.string().nullable(),
  workerStatus: z.string().nullable(),
  hasPr: z.boolean(),
  prReviewState: z.string().nullable(),
  ciStatus: z.string().nullable(),
  mergeableStatus: z.string().nullable(),
  labels: z.array(z.string()),
});

export const HealthTickEventSchema = FeedbackEventBase.extend({
  event: z.literal("daemon.health_tick"),
  tick: z.number().int().nonnegative(),
  workerCount: z.number().int().nonnegative(),
  serveHealthy: z.boolean(),
  uptimeS: z.number().nonnegative(),
  serveRestarted: z.boolean(),
  sessionsRecreated: z.number().int().nonnegative(),
  rssRestarts: z.number().int().nonnegative(),
});

export const WorkerReapedEventSchema = FeedbackEventBase.extend({
  event: z.literal("daemon.worker_reaped"),
  workerId: z.string(),
  sessionId: z.string(),
  mode: z.string(),
  serveType: z.string(),
  reason: z.string(),
});

export const FeedbackEventSchema = z.discriminatedUnion("event", [
  WorkerDispatchedEventSchema,
  WorkerStatusChangedEventSchema,
  StateCollectedEventSchema,
  HealthTickEventSchema,
  WorkerReapedEventSchema,
]);

type WorkerDispatchedEvent = z.infer<typeof WorkerDispatchedEventSchema>;
type WorkerStatusChangedEvent = z.infer<typeof WorkerStatusChangedEventSchema>;
type StateCollectedEvent = z.infer<typeof StateCollectedEventSchema>;
type HealthTickEvent = z.infer<typeof HealthTickEventSchema> & {
  issueId?: undefined;
};
type WorkerReapedEvent = z.infer<typeof WorkerReapedEventSchema>;

export type FeedbackEvent =
  | WorkerDispatchedEvent
  | WorkerStatusChangedEvent
  | StateCollectedEvent
  | HealthTickEvent
  | WorkerReapedEvent;

type OmitFeedbackBase<TEvent> = TEvent extends unknown
  ? Omit<TEvent, "schemaVersion" | "timestamp" | "legionId">
  : never;

type QueuedFeedbackEvent = OmitFeedbackBase<FeedbackEvent>;

export interface FeedbackWriter {
  append(line: string): Promise<void>;
  flush(): Promise<void>;
}

export class FileFeedbackWriter implements FeedbackWriter {
  constructor(
    private readonly filePath: string,
    private readonly maxBytes: number = 50 * 1024 * 1024
  ) {}

  async append(line: string): Promise<void> {
    try {
      const fileStats = await stat(this.filePath).catch(() => null);
      if (fileStats && fileStats.size >= this.maxBytes) {
        const backupPath = `${this.filePath}.1`;
        try {
          await unlink(backupPath);
        } catch {}
        await rename(this.filePath, backupPath);
      }
    } catch {}

    await appendFile(this.filePath, `${line}\n`);
  }

  async flush(): Promise<void> {}
}

export class FeedbackLogger {
  private queue: string[] = [];
  private drainPromise: Promise<void> | null = null;

  constructor(
    private readonly writer: FeedbackWriter,
    private readonly legionId: string
  ) {}

  log(event: QueuedFeedbackEvent): void {
    const fullEvent = {
      schemaVersion: 1 as const,
      timestamp: new Date().toISOString(),
      legionId: this.legionId,
      ...event,
    };

    const result = FeedbackEventSchema.safeParse(fullEvent);
    if (!result.success) {
      console.error(`[feedback] Invalid event dropped: ${result.error.message}`);
      return;
    }

    this.queue.push(JSON.stringify(result.data));
    this.scheduleDrain();
  }

  private scheduleDrain(): void {
    if (this.drainPromise) {
      return;
    }

    this.drainPromise = this.drainLoop().finally(() => {
      this.drainPromise = null;
      if (this.queue.length > 0) {
        this.scheduleDrain();
      }
    });
  }

  private async drainLoop(): Promise<void> {
    while (this.queue.length > 0) {
      const line = this.queue.shift();
      if (!line) {
        continue;
      }

      try {
        await this.writer.append(line);
      } catch (error) {
        console.error(`[feedback] Write failed: ${error}`);
      }
    }
  }

  async flush(): Promise<void> {
    while (this.queue.length > 0 || this.drainPromise) {
      if (this.drainPromise) {
        await this.drainPromise;
      }

      if (this.queue.length > 0) {
        await this.drainLoop();
      }
    }

    await this.writer.flush();
  }
}

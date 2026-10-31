/**
 * One Web Worker, one job at a time, for the two surfaces of this folder that own one: the parse
 * worker a long Markdown body goes to (`markdown-engine.ts`) and the renderer a mermaid diagram
 * goes to (`mermaid-diagrams.ts`). Both wanted the same queue - a ready handshake before the
 * first job, one job in flight, a deadline that starts when the worker can begin, terminate and
 * replace, drop what nobody waits for on a microtask, ignore an answer to a job already dropped -
 * and had written it twice, so a fix to either reached one of them (sjawhar/legion#2519). They
 * speak one protocol too (`WorkerReply`), so the queue reads a reply's `ready` and `id` itself
 * and neither owner writes that twice.
 *
 * What differs is the policy, which `WorkerQueuePolicy` carries: what the worker is, what a job
 * is posted as, and above all what becomes of a job the worker cannot answer. The diagram
 * renderer refuses such a source, because the only thing it could do on the page is the layout
 * the worker could not finish; the parse falls back to the page's own thread, because a body
 * must end formatted whatever happened to the worker.
 *
 * A job the worker is already holding cannot be taken back, so a job the page has moved on from
 * is the one place the two still differ (`stopAnAbandonedJob`): a diagram's layout can run for
 * seconds, so the renderer is terminated and replaced, while a fresh parse worker costs more
 * than the parse it would save - the worker holds a module of its own and builds a headless
 * engine before it can answer anything - so the parse queue lets the worker finish. Its answer
 * is dropped where nobody wants it, and kept where the owner asked to keep a posted job
 * (`keepIfPosted`), which is how a streaming turn is shown the frame the worker was holding.
 * Either way the next job waits for that one rather than being posted behind it: a turn
 * streaming into the live view hands the worker one stale frame, never one per frame.
 */

/** Every job carries the key it is queued and cached under: two renders of the same body, or two
 *  blocks of the same diagram, wait on one job. */
export interface WorkerJob {
  readonly key: string;
}

/**
 * What a worker of this folder posts, which is the one protocol both of them speak: the ready
 * handshake, once its imports have loaded and before it answers anything, and then one answer
 * per job, naming the job by the id it was posted under. `Answered` is what the worker has to
 * say about a job, which its owner's policy turns into the answer the page keeps
 * (`WorkerQueuePolicy.answerOf`); the queue itself reads nothing of a reply but `ready` and
 * `id`.
 */
export type WorkerReply<Answered> = { ready: true } | ({ id: number } & Answered);

/** One render's place on a job: the waiter its answer goes to, and whether the render that
 *  asked has gone while the worker held the job (`keepIfPosted`). A kept place is worth the
 *  worker's own answer, which is what `keepIfPosted` buys, and nothing else: the render was
 *  cancelled, so an answer the queue makes without the worker would be work nobody reads - for
 *  the parse, a parse of an abandoned body on the reader's own thread (sjawhar/legion#2519). */
interface Waiter<Answer> {
  readonly answer: (answer: Answer) => void;
  kept: boolean;
}

/** One job in the queue: what its owner knows about it, the id it was posted under, and every
 *  render waiting on its answer. */
interface Queued<Job extends WorkerJob, Answer> {
  readonly id: number;
  readonly job: Job;
  /** A job two surfaces asked for at once is posted once; a job none waits for any more is
   *  abandoned. */
  readonly waiters: Waiter<Answer>[];
}

/** Whether any render still waits on `entry` for an answer of whatever kind, rather than only
 *  for the worker's own (`Waiter`). */
function anyoneIsWaiting<Job extends WorkerJob, Answer>(entry: Queued<Job, Answer>): boolean {
  return entry.waiters.some((waiter) => !waiter.kept);
}

/** What a queue does that its two owners do differently. */
export interface WorkerQueuePolicy<Job extends WorkerJob, Answered, Answer> {
  /**
   * Terminates the worker holding a job nobody waits for any more, rather than letting it
   * finish. True where the work the worker is doing costs more than a fresh worker does, false
   * where the worker itself is the expensive thing.
   */
  readonly stopAnAbandonedJob: boolean;
  /** Creates the worker `first` and every job behind it goes to, posting whatever it needs
   *  before its first job. A throw leaves the queue with no worker (`noWorkerFrom`). */
  createWorker(first: Job): Worker;
  /** Posts `job` under `id`, which names its answer. */
  post(worker: Worker, id: number, job: Job): void;
  /** `job`'s answer as the worker's reply carries it, which the policy caches. The queue has
   *  already matched the reply to `job` by its id, so what is left is the owner's own reading of
   *  it; an owner that cannot take the answer answers as it does without the worker. */
  answerOf(job: Job, reply: { id: number } & Answered): Answer;
  /** `job`'s answer where the worker that held it gave none: its deadline ran out, which has
   *  terminated that worker first, so the jobs behind it go to a fresh one. */
  withoutTheWorker(job: Job): Answer;
  /** `job`'s answer where no worker will run at all: one could not be created, its chunk did not
   *  load, or one crashed. */
  withoutAWorker(job: Job): Answer;
  /** Told once, when the queue gives up on having a worker at all, so the owner can report it
   *  and arrange whatever brings one back (`recover`). */
  noWorkerFrom(error: unknown): void;
  /** How long the worker may hold one job, read per job so a test can shorten it. */
  limitMs(): number;
  /** Told after every change that can have emptied the queue, for an owner that answers whether
   *  its work has settled. */
  settled?(): void;
}

export class WorkerQueue<Job extends WorkerJob, Answered, Answer> {
  private readonly policy: WorkerQueuePolicy<Job, Answered, Answer>;
  /** The jobs waiting to be posted, oldest first; the one in flight is not among them. */
  private queued: Queued<Job, Answer>[] = [];
  /** The job the worker is holding: posted and not answered. Nothing else is posted until it is
   *  answered, its deadline runs out, or the worker is replaced. */
  private inFlight: Queued<Job, Answer> | undefined;
  private worker: Worker | undefined;
  /** The worker has posted its ready handshake: it starts on a job as soon as it has one. */
  private ready = false;
  /** The deadline of the job in flight. */
  private timer: number | undefined;
  /** No worker will run for this page until `recover`. */
  private noWorker = false;
  /** A cancel has queued `dropAbandoned` and it has not run yet. */
  private dropQueued = false;
  private lastId = 0;

  constructor(policy: WorkerQueuePolicy<Job, Answered, Answer>) {
    this.policy = policy;
  }

  /** Whether any job is waiting for the worker, the one in flight included. */
  get waiting(): boolean {
    return this.inFlight !== undefined || this.queued.length > 0;
  }

  /**
   * Adds `waiter` to the job under `key`, queuing the job `make` builds where nothing waits for
   * it yet. Where no worker will run at all, `waiter` is called inside this call with the answer
   * the policy makes without one.
   *
   * Returns the cancel: it takes `waiter` off the job, so a cancelled render never runs and
   * holds none of its caller's DOM while the worker works, and then queues the drop on a
   * microtask, after any re-render in the same commit has asked again.
   *
   * `keepIfPosted` is for an owner whose answer is worth having even once it has asked for
   * something newer: a cancel then leaves `waiter` on a job the worker has already been posted,
   * so that answer still reaches it, and takes it only off a job still waiting to be posted,
   * which is dropped as any other. A turn streaming into the live view asks that way
   * (`markdown-engine.ts`): every frame cancels the frame before it, and the frame the worker is
   * holding carries the only parse the reader can be shown before the stream ends
   * (sjawhar/legion#2519). Such an owner must expect an answer to a source it has moved on from,
   * and must say for itself when that answer stops being worth painting. The place a cancel
   * leaves that way is worth the worker's own answer alone (`Waiter`): a deadline, or a worker
   * that never runs, answers the job for whoever is still waiting on it and leaves that place
   * unanswered, so a body the page has moved on from is never worked on here instead.
   */
  wait(
    key: string,
    make: () => Job,
    waiter: (answer: Answer) => void,
    { keepIfPosted = false }: { keepIfPosted?: boolean } = {}
  ): () => void {
    if (this.noWorker) {
      waiter(this.policy.withoutAWorker(make()));
      return () => undefined;
    }
    let entry = this.queued.find((queued) => queued.job.key === key);
    if (entry === undefined && this.inFlight?.job.key === key) {
      entry = this.inFlight;
    }
    const place: Waiter<Answer> = { answer: waiter, kept: false };
    if (entry === undefined) {
      this.lastId += 1;
      entry = { id: this.lastId, job: make(), waiters: [place] };
      this.queued.push(entry);
      this.postNext();
    } else {
      entry.waiters.push(place);
    }
    const waiting = entry;
    let cancelled = false;
    return () => {
      if (cancelled) {
        return;
      }
      cancelled = true;
      if (keepIfPosted && this.inFlight === waiting) {
        place.kept = true;
        return;
      }
      const at = waiting.waiters.indexOf(place);
      if (at !== -1) {
        waiting.waiters.splice(at, 1);
      }
      if (waiting.waiters.length > 0 || this.dropQueued) {
        return;
      }
      this.dropQueued = true;
      queueMicrotask(() => {
        this.dropAbandoned();
      });
    };
  }

  /** Lets the queue start a worker again: the window came back online, or a test replaced the
   *  factory the policy creates one with. */
  recover(): void {
    this.noWorker = false;
  }

  /**
   * Terminates the worker and takes every job out of the queue: an answer the worker posted
   * before is dropped, and the next job starts a fresh one. `settleWith`, where it is given,
   * answers every job the queue held, as the worker now never will; without it the jobs are
   * simply forgotten, which is what a test resetting the page wants.
   */
  clear(settleWith?: (job: Job) => Answer): void {
    const held = this.stopWorker();
    this.queued = [];
    if (settleWith !== undefined) {
      for (const entry of held) {
        this.settle(entry, settleWith(entry.job));
      }
    }
    this.policy.settled?.();
  }

  /** Terminates the worker and forgets it and the job it held, which this answers; the jobs
   *  behind it stay queued for the next one. */
  private stopWorker(): Queued<Job, Answer>[] {
    window.clearTimeout(this.timer);
    this.timer = undefined;
    const held = this.inFlight;
    this.inFlight = undefined;
    this.worker?.terminate();
    this.worker = undefined;
    this.ready = false;
    return held === undefined ? [...this.queued] : [held, ...this.queued];
  }

  /** Posts the oldest waiting job, once a worker is ready for one and holds nothing else. */
  private postNext(): void {
    const next = this.queued[0];
    if (next === undefined || this.inFlight !== undefined || this.noWorker) {
      return;
    }
    if (this.worker === undefined) {
      this.startWorker(next.job);
      return;
    }
    if (!this.ready) {
      return;
    }
    this.queued.shift();
    this.inFlight = next;
    this.timer = window.setTimeout(() => {
      this.expire(next);
    }, this.policy.limitMs());
    this.policy.post(this.worker, next.id, next.job);
  }

  private startWorker(first: Job): void {
    let started: Worker;
    try {
      started = this.policy.createWorker(first);
    } catch (error) {
      this.giveUp(error);
      return;
    }
    this.worker = started;
    this.ready = false;
    started.addEventListener("message", (event: MessageEvent<WorkerReply<Answered>>) => {
      this.answered(started, event.data);
    });
    started.addEventListener("error", (event) => {
      if (started === this.worker) {
        this.giveUp(event);
      }
    });
  }

  private answered(from: Worker, reply: WorkerReply<Answered>): void {
    // An answer from a worker since replaced says nothing about the job a fresh one holds.
    if (from !== this.worker) {
      return;
    }
    if ("ready" in reply) {
      this.ready = true;
      this.postNext();
      return;
    }
    const held = this.inFlight;
    if (held === undefined || reply.id !== held.id) {
      return;
    }
    window.clearTimeout(this.timer);
    this.timer = undefined;
    this.inFlight = undefined;
    // The worker starts on the next job while this answer is turned into what the page needs.
    this.postNext();
    // Nothing is kept for a job the page has moved on from: the answer is dropped where it
    // arrives, rather than costing the thread the work of taking it and the cache a place. A
    // waiter that asked to keep a posted job (`keepIfPosted`) is still one, so a streaming turn
    // is shown the frame the worker was holding.
    if (held.waiters.length > 0) {
      this.settle(held, this.policy.answerOf(held.job, reply));
    }
    this.policy.settled?.();
  }

  /** The worker has not answered `held` within the limit: it is stopped, which stops that work,
   *  `held` is settled without it for whoever still waits on it, and the jobs behind it go to a
   *  fresh worker. */
  private expire(held: Queued<Job, Answer>): void {
    if (held !== this.inFlight) {
      return;
    }
    this.stopWorker();
    // What the policy does without the worker is for a render still waiting on this job - for
    // the parse, the reader's own thread, which is where a body must end formatted. A place
    // kept on a posted job is not one (`Waiter`): the deadline of a body whose element has gone
    // would otherwise park that parse on the thread of a reader who left it
    // (sjawhar/legion#2519).
    if (anyoneIsWaiting(held)) {
      this.settle(held, this.policy.withoutTheWorker(held.job));
    }
    this.postNext();
    this.policy.settled?.();
  }

  /** No worker will run: every job someone still waits on is answered without one. */
  private giveUp(error: unknown): void {
    this.noWorker = true;
    const held = this.stopWorker();
    this.queued = [];
    this.policy.noWorkerFrom(error);
    for (const entry of held) {
      if (anyoneIsWaiting(entry)) {
        this.settle(entry, this.policy.withoutAWorker(entry.job));
      }
    }
    this.policy.settled?.();
  }

  private settle(entry: Queued<Job, Answer>, answer: Answer): void {
    for (const waiter of [...entry.waiters]) {
      waiter.answer(answer);
    }
  }

  /** Drops every job no render waits for any more, so a turn streaming into the live view leaves
   *  the worker only the frame someone is reading. It runs on the microtask after a cancel, not
   *  in it: React runs a layout effect's cleanup and the next run of it in one commit, so a job
   *  the same commit asks for again keeps its place in the queue, and its answer. */
  private dropAbandoned(): void {
    this.dropQueued = false;
    this.queued = this.queued.filter((entry) => anyoneIsWaiting(entry));
    if (
      this.inFlight !== undefined &&
      !anyoneIsWaiting(this.inFlight) &&
      this.policy.stopAnAbandonedJob
    ) {
      this.stopWorker();
    }
    this.postNext();
    this.policy.settled?.();
  }
}

/** Keeps `value` under `key` in a cache of at most `limit` entries, dropping the entry inserted
 *  longest ago where it is full. A cache whose reads re-insert what they read (`Map` keeps its
 *  insertion order) drops the entry read longest ago instead. */
export function rememberUpTo<Value>(
  cache: Map<string, Value>,
  limit: number,
  key: string,
  value: Value
): void {
  if (cache.size >= limit) {
    const oldest = cache.keys().next();
    if (oldest.done !== true) cache.delete(oldest.value);
  }
  cache.set(key, value);
}

import { heapStats } from "bun:jsc";
import * as v8 from "node:v8";

type GaugeFn = () => Record<string, number>;

type Snap = {
  at: string;
  rss: number;
  heapTotal: number;
  heapUsed: number;
  objectCount: number;
  types: Record<string, number>;
  gauges: Record<string, number>;
};

const SAMPLE_MS = 5_000;
const SUMMARY_EVERY = 12;
const GROWTH_LIMIT = 50 * 1024 * 1024;

const sources = new Map<string, GaugeFn>();

let timer: ReturnType<typeof setInterval> | undefined;
let prev: Snap | undefined;
let base: Snap | undefined;
let tick = 0;
let takingSnapshot = false;
let usr1: (() => void) | undefined;
let usr2: (() => void) | undefined;

function toMB(value: number) {
  return Number((value / 1024 / 1024).toFixed(1));
}

function emit(event: string, fields: Record<string, string | number | undefined>) {
  const body = Object.entries(fields)
    .filter(([, value]) => value !== undefined)
    .map(([key, value]) => `${key}=${value}`)
    .join(" ");
  process.stderr.write(`[daemon-telemetry] event=${event}${body ? ` ${body}` : ""}\n`);
}

export function registerGauges(key: string, fn: GaugeFn) {
  sources.set(key, fn);
  return () => {
    sources.delete(key);
  };
}

export function readGauges(): Record<string, number> {
  const merged: Record<string, number> = {};
  for (const source of sources.values()) {
    Object.assign(merged, source());
  }
  return merged;
}

export function takeSnapshot(): Snap {
  const mem = process.memoryUsage();
  const stat = heapStats();
  const types = Object.fromEntries(
    Object.entries(stat.objectTypeCounts).map(([k, v]) => [k, Number(v)])
  );
  return {
    at: new Date().toISOString(),
    rss: mem.rss,
    heapTotal: mem.heapTotal,
    heapUsed: mem.heapUsed,
    objectCount: Number(stat.objectCount),
    types,
    gauges: readGauges(),
  };
}

function sampleOnce(event: "summary" | "growth" | "signal") {
  const next = takeSnapshot();
  const last = prev;
  const start = base;
  const delta = last ? next.rss - last.rss : 0;
  emit(`memory.${event}`, {
    ts: next.at,
    rss_mb: toMB(next.rss),
    heap_mb: toMB(next.heapUsed),
    heap_total_mb: toMB(next.heapTotal),
    object_count: next.objectCount,
    rss_delta_mb: last ? toMB(delta) : 0,
    rss_since_start_mb: start ? toMB(next.rss - start.rss) : 0,
    ...next.gauges,
  });
  prev = next;
  if (!base) base = next;
}

function maybeSample() {
  const next = takeSnapshot();
  const last = prev;
  const start = base;
  const delta = last ? next.rss - last.rss : 0;
  const summary = tick % SUMMARY_EVERY === 0;
  const growing = tick >= SUMMARY_EVERY && delta > GROWTH_LIMIT;
  if (summary || growing) {
    emit(summary ? "memory.summary" : "memory.growth", {
      ts: next.at,
      rss_mb: toMB(next.rss),
      heap_mb: toMB(next.heapUsed),
      heap_total_mb: toMB(next.heapTotal),
      object_count: next.objectCount,
      rss_delta_mb: last ? toMB(delta) : 0,
      rss_since_start_mb: start ? toMB(next.rss - start.rss) : 0,
      ...next.gauges,
    });
  }
  prev = next;
  if (!base) base = next;
  tick += 1;
}

function writeSnapshot(signal: string) {
  if (takingSnapshot) return;
  takingSnapshot = true;
  try {
    const path = `/tmp/legion-heap-${process.pid}-${Date.now()}.heapsnapshot`;
    emit("memory.snapshot_start", {
      signal,
      path,
      rss_mb: toMB(process.memoryUsage().rss),
    });
    const file = v8.writeHeapSnapshot(path);
    emit("memory.snapshot_done", { signal, path: file });
  } finally {
    takingSnapshot = false;
  }
}

export function registerSignals() {
  if (usr1 && usr2) return;
  usr1 = () => writeSnapshot("SIGUSR1");
  usr2 = () => sampleOnce("signal");
  process.on("SIGUSR1", usr1);
  process.on("SIGUSR2", usr2);
}

export function start() {
  registerSignals();
  if (timer) return;
  tick = 0;
  prev = undefined;
  base = undefined;
  const first = takeSnapshot();
  prev = first;
  base = first;
  emit("memory.baseline", {
    ts: first.at,
    rss_mb: toMB(first.rss),
    heap_mb: toMB(first.heapUsed),
    heap_total_mb: toMB(first.heapTotal),
    object_count: first.objectCount,
    ...first.gauges,
  });
  timer = setInterval(maybeSample, SAMPLE_MS);
  timer.unref?.();
}

export function stop() {
  if (!timer) return;
  clearInterval(timer);
  timer = undefined;
}

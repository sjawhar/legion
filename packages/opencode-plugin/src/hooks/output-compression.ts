import type { OutputCompressionConfig } from "../config";
import { ContentStore } from "../store/content-store";
import { isRecord, resolveSessionID } from "./utils";

const DEFAULT_THRESHOLD_BYTES = 5000;
const DEFAULT_MAX_INDEX_SIZE_MB = 50;
const DEFAULT_EXCLUDED_TOOLS = new Set([
  "edit",
  "write",
  "slashcommand",
  "context_search",
  "todowrite",
  "switch_agent",
]);

// LINE#ID prefixes (e.g. "734#QH|") are added by the Read tool for the Edit tool.
// Strip them before indexing to avoid polluting FTS with line numbers and hash tags.
const LINE_ID_PATTERN = /^\d+#[A-Z]{2}\|/gm;

export interface CompressionStats {
  compressed: number;
  bytesSaved: number;
  passedThrough: number;
}

function isErrorLikeOutput(text: string): boolean {
  const trimmed = text.trimStart();
  if (trimmed.startsWith("Error:") || trimmed.startsWith("error:")) {
    return true;
  }
  if (/(^|\n)\s*at\s+.+\(.+:\d+:\d+\)/m.test(text) || /(^|\n)\s*at\s+.+:\d+:\d+/m.test(text)) {
    return true;
  }
  const head = text.slice(0, 2000);
  if (/Traceback \(most recent call last\)/.test(head)) {
    return true;
  }
  if (/^goroutine \d+ \[/m.test(head)) {
    return true;
  }
  if (/^thread '.+' panicked at/m.test(head)) {
    return true;
  }
  return false;
}

export function createOutputCompressionHook(config: OutputCompressionConfig = {}) {
  let store: ContentStore | null = null;
  const getOrCreateStore = (): ContentStore => {
    if (!store) {
      store = new ContentStore({ maxSizeMB: config.maxIndexSizeMB ?? DEFAULT_MAX_INDEX_SIZE_MB });
    }
    return store;
  };
  const thresholdBytes = config.thresholdBytes ?? DEFAULT_THRESHOLD_BYTES;
  const excludedTools = new Set([...DEFAULT_EXCLUDED_TOOLS, ...(config.excludeTools ?? [])]);

  const stats: CompressionStats = {
    compressed: 0,
    bytesSaved: 0,
    passedThrough: 0,
  };

  const toolExecuteAfter = (
    input: { tool: string; sessionID: string; callID: string; args?: unknown },
    output: { title: string; output: string; metadata: unknown }
  ) => {
    if (config.enabled === false) {
      stats.passedThrough += 1;
      return;
    }

    if (excludedTools.has(input.tool)) {
      stats.passedThrough += 1;
      return;
    }

    if (typeof output.output !== "string") {
      stats.passedThrough++;
      return;
    }

    const rawOutput = output.output ?? "";
    const byteCount = Buffer.byteLength(rawOutput, "utf8");
    if (byteCount < thresholdBytes) {
      stats.passedThrough += 1;
      return;
    }

    if (isErrorLikeOutput(rawOutput)) {
      stats.passedThrough += 1;
      return;
    }

    const source = `${input.sessionID}:${input.tool}:${input.callID}`;
    try {
      const activeStore = getOrCreateStore();
      const cleanContent = rawOutput.replace(LINE_ID_PATTERN, "");
      const indexed = activeStore.index({
        content: cleanContent,
        source,
        session: input.sessionID,
      });
      const topTerms = indexed.vocabulary.join(", ");
      const suggestedQueries = indexed.vocabulary.slice(0, 3).join(", ");
      output.output = [
        `[Compressed] ${byteCount} bytes from ${input.tool} indexed as "${source}".`,
        `${indexed.chunkCount} sections indexed. Top terms: ${topTerms}.`,
        `Use context_search tool with queries like [${suggestedQueries}] to retrieve specific sections.`,
      ].join("\n");
      stats.compressed += 1;
      stats.bytesSaved += Math.max(0, byteCount - Buffer.byteLength(output.output, "utf8"));
    } catch (error) {
      console.warn("[legion-plugin] Output compression failed:", error);
      stats.passedThrough += 1;
    }
  };

  const event = async ({
    event,
  }: {
    event: { type: string; properties?: unknown };
  }): Promise<void> => {
    const props = isRecord(event.properties) ? event.properties : undefined;

    if (event.type === "session.deleted") {
      const sessionID = resolveSessionID(props);
      if (sessionID && store) {
        store.deleteSession(sessionID);
      }
    }
  };

  // Clean up SQLite DB on process exit (DB is process-scoped)
  const cleanup = () => {
    try {
      store?.close();
    } catch {
      // best-effort cleanup
    }
  };
  process.once("exit", cleanup);

  return {
    "tool.execute.after": toolExecuteAfter,
    event,
    getStore: () => store,
    getStats: (): CompressionStats => ({ ...stats }),
    cleanup,
  };
}

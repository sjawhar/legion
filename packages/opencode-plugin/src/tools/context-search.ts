import type { ToolContext, ToolDefinition } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import type { ContentStore } from "../store/content-store";

const z = tool.schema;

export function createContextSearchTool(store: ContentStore): ToolDefinition {
  return tool({
    description:
      "Search content indexed from previous large tool outputs. " +
      "Use when a tool output was compressed and you need specific details. " +
      "Note: Search uses word tokenization, so file paths, flags (--verbose), and symbols (Error:) are split into individual words. " +
      "Use words from paths as search terms for best results.",
    args: {
      queries: z.array(z.string()).min(1).max(10).describe("Search queries (batch multiple in one call)"),
      source: z
        .string()
        .optional()
        .describe("Filter to specific tool output (e.g. 'sessionID:tool:callID')"),
      limit: z.number().int().min(1).max(10).optional().describe("Results per query (default: 3)"),
    },
    execute: async (args, ctx: ToolContext): Promise<string> => {
      try {
        const typedArgs = args as { queries: string[]; source?: string; limit?: number };
        const limit = typedArgs.limit ?? 3;
        const results = store.search({
          queries: typedArgs.queries,
          source: typedArgs.source,
          session: ctx.sessionID,
          limit,
        });

        if (results.length === 0) {
          return "No matching content found. Try different search terms or check available sources with context_search.";
        }

        return results
          .map(
            (result) => {
              const content = result.content.length > 2000
                ? result.content.slice(0, 2000) + "\n[...truncated — use a more specific query to get full content]"
                : result.content;
              return `--- [${result.source}] ${result.title} (score: ${result.score.toFixed(2)}) ---\n${content}`;
            }
          )
          .join("\n\n");
      } catch (error) {
        return `Error: ${error instanceof Error ? error.message : String(error)}`;
      }
    },
  });
}

import type { ToolContext, ToolDefinition } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import type { ContentStore } from "../store/content-store";

const z = tool.schema;

export function createContextSearchTool(store: ContentStore): ToolDefinition {
  return tool({
    description:
      "Search content indexed from previous large tool outputs. " +
      "Use when a tool output was compressed and you need specific details.",
    args: {
      queries: z.array(z.string()).describe("Search queries (batch multiple in one call)"),
      source: z
        .string()
        .optional()
        .describe("Filter to specific tool output (e.g. 'bash:call123')"),
      limit: z.number().optional().describe("Results per query (default: 3)"),
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
            (result) =>
              `--- [${result.source}] ${result.title} (score: ${result.score.toFixed(2)}) ---\n${result.content}`
          )
          .join("\n\n");
      } catch (error) {
        return `Error: ${error instanceof Error ? error.message : String(error)}`;
      }
    },
  });
}

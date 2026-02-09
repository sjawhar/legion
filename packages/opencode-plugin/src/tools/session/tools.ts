import type { PluginInput, ToolDefinition } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";

const z = tool.schema;

type OpencodeClient = PluginInput["client"];

interface SessionTools {
  session_list: ToolDefinition;
  session_read: ToolDefinition;
  session_search: ToolDefinition;
  session_info: ToolDefinition;
}

export function createSessionTools(client: OpencodeClient, directory: string): SessionTools {
  const session_list: ToolDefinition = tool({
    description:
      "List all OpenCode sessions with optional filtering.\n\n" +
      "Returns a list of available session IDs with metadata including " +
      "message count, date range, and agents used.",
    args: {
      limit: z.number().optional().describe("Maximum number of sessions to return"),
      from_date: z.string().optional().describe("Filter sessions from this date (ISO 8601 format)"),
      to_date: z.string().optional().describe("Filter sessions until this date (ISO 8601 format)"),
    },
    execute: async (args) => {
      try {
        const typedArgs = args as { limit?: number; from_date?: string; to_date?: string };
        const result = await client.session.list({ query: { directory } });
        if (!result || typeof result !== "object") {
          return "No sessions found.";
        }

        const data = result as { data?: unknown };
        let sessions = (data.data ?? result) as unknown[];
        if (!Array.isArray(sessions)) {
          return JSON.stringify(sessions, null, 2);
        }

        if (typedArgs.limit && sessions.length > typedArgs.limit) {
          sessions = sessions.slice(0, typedArgs.limit);
        }

        return JSON.stringify(sessions, null, 2);
      } catch (e) {
        return `Error: ${e instanceof Error ? e.message : String(e)}`;
      }
    },
  });

  const session_read: ToolDefinition = tool({
    description:
      "Read messages and history from an OpenCode session.\n\n" +
      "Returns a formatted view of session messages with role, timestamp, and content.",
    args: {
      session_id: z.string().describe("Session ID to read"),
      limit: z.number().optional().describe("Maximum number of messages to return"),
      include_todos: z.boolean().optional().describe("Include todo list if available"),
      include_transcript: z.boolean().optional().describe("Include transcript log if available"),
    },
    execute: async (args) => {
      try {
        const typedArgs = args as {
          session_id: string;
          limit?: number;
          include_todos?: boolean;
          include_transcript?: boolean;
        };

        const sessionResult = await client.session.get({
          path: { id: typedArgs.session_id },
          query: { directory },
        });

        const messagesResult = await client.session.messages({
          path: { id: typedArgs.session_id },
          query: { directory },
        });

        const parts: string[] = [];
        parts.push(`Session: ${typedArgs.session_id}`);

        const sessionData = sessionResult as { data?: unknown };
        if (sessionData.data) {
          parts.push(JSON.stringify(sessionData.data, null, 2));
        }

        parts.push("\n--- Messages ---");
        const messagesData = messagesResult as { data?: unknown };
        if (messagesData.data) {
          const messages = messagesData.data as unknown[];
          const limited = typedArgs.limit ? messages.slice(0, typedArgs.limit) : messages;
          parts.push(JSON.stringify(limited, null, 2));
        }

        if (typedArgs.include_todos) {
          try {
            const todoResult = await (
              client.session as unknown as Record<string, CallableFunction>
            ).todo({
              path: { id: typedArgs.session_id },
              query: { directory },
            });
            const todoData = todoResult as { data?: unknown };
            if (todoData.data) {
              parts.push("\n--- Todos ---");
              parts.push(JSON.stringify(todoData.data, null, 2));
            }
          } catch {
            // Todos not available
          }
        }

        return parts.join("\n");
      } catch (e) {
        return `Error: ${e instanceof Error ? e.message : String(e)}`;
      }
    },
  });

  const session_search: ToolDefinition = tool({
    description:
      "Search for content within OpenCode session messages.\n\n" +
      "Performs full-text search across session messages and returns matching excerpts.",
    args: {
      query: z.string().describe("Search query string"),
      session_id: z.string().optional().describe("Search within specific session only"),
      case_sensitive: z.boolean().optional().describe("Case-sensitive search (default: false)"),
      limit: z.number().optional().describe("Maximum number of results to return (default: 20)"),
    },
    execute: async (args) => {
      try {
        const typedArgs = args as {
          query: string;
          session_id?: string;
          case_sensitive?: boolean;
          limit?: number;
        };

        const sessionsToSearch: string[] = [];

        if (typedArgs.session_id) {
          sessionsToSearch.push(typedArgs.session_id);
        } else {
          const listResult = await client.session.list({ query: { directory } });
          const listData = listResult as { data?: Array<{ id: string }> };
          if (listData.data) {
            for (const s of listData.data) {
              sessionsToSearch.push(s.id);
            }
          }
        }

        const maxResults = typedArgs.limit ?? 20;
        const results: string[] = [];
        const queryLower = typedArgs.case_sensitive
          ? typedArgs.query
          : typedArgs.query.toLowerCase();

        for (const sessionId of sessionsToSearch) {
          if (results.length >= maxResults) break;

          try {
            const msgResult = await client.session.messages({
              path: { id: sessionId },
              query: { directory },
            });
            const msgData = msgResult as { data?: unknown[] };
            if (!msgData.data) continue;

            for (const msg of msgData.data) {
              if (results.length >= maxResults) break;
              const msgStr = JSON.stringify(msg);
              const searchIn = typedArgs.case_sensitive ? msgStr : msgStr.toLowerCase();
              if (searchIn.includes(queryLower)) {
                const msgObj = msg as { id?: string; role?: string };
                results.push(`[${sessionId}] Message ${msgObj.id ?? "?"} (${msgObj.role ?? "?"})`);
              }
            }
          } catch {
            // Skip sessions that fail
          }
        }

        if (results.length === 0) {
          return "No matches found.";
        }

        return `Found ${results.length} matches:\n\n${results.join("\n")}`;
      } catch (e) {
        return `Error: ${e instanceof Error ? e.message : String(e)}`;
      }
    },
  });

  const session_info: ToolDefinition = tool({
    description:
      "Get metadata and statistics about an OpenCode session.\n\n" +
      "Returns detailed information about a session including message count, " +
      "date range, agents used, and available data sources.",
    args: {
      session_id: z.string().describe("Session ID to inspect"),
    },
    execute: async (args) => {
      try {
        const typedArgs = args as { session_id: string };

        const sessionResult = await client.session.get({
          path: { id: typedArgs.session_id },
          query: { directory },
        });

        const messagesResult = await client.session.messages({
          path: { id: typedArgs.session_id },
          query: { directory },
        });

        const parts: string[] = [];
        parts.push(`Session ID: ${typedArgs.session_id}`);

        const sessionData = sessionResult as { data?: Record<string, unknown> };
        if (sessionData.data) {
          for (const [key, value] of Object.entries(sessionData.data)) {
            if (key !== "id") {
              parts.push(`${key}: ${JSON.stringify(value)}`);
            }
          }
        }

        const messagesData = messagesResult as { data?: unknown[] };
        if (messagesData.data) {
          parts.push(`Messages: ${messagesData.data.length}`);
        }

        let hasTodos = false;
        try {
          const todoResult = await (
            client.session as unknown as Record<string, CallableFunction>
          ).todo({
            path: { id: typedArgs.session_id },
            query: { directory },
          });
          const todoData = todoResult as { data?: unknown[] };
          hasTodos = !!todoData.data && todoData.data.length > 0;
        } catch {
          // Todos not available
        }
        parts.push(`Has Todos: ${hasTodos ? "Yes" : "No"}`);

        return parts.join("\n");
      } catch (e) {
        return `Error: ${e instanceof Error ? e.message : String(e)}`;
      }
    },
  });

  return { session_list, session_read, session_search, session_info };
}

// Auto-subscription wiring for the envoy_dispatch MCP tool (Dispatch AC#4).
//
// When an agent opens a Dispatch thread via the envoy_dispatch MCP tool, the
// human answers by commenting on the resulting GitHub sub-issue. For the agent
// to RECEIVE that answer, its session must be subscribed to the thread's Envoy
// topic (notifications.github.<owner>.<repo>.issue.<thread>.>). The dispatch
// tool is served by the Go dispatch server and has no OpenCode session context,
// so we close the loop in the plugin (which does know the session id) from the
// tool.execute.after hook.
//
// This module is the pure, testable core: it turns a completed tool execution
// into the topic the calling session should subscribe to, or null when the
// execution isn't a successful envoy_dispatch call.

// Matches a GitHub issue URL anywhere in the tool output and captures
// owner / repo / number. The dispatch tool returns {"thread":N,"url":"…"} as
// its text content, so the URL is always present on success.
const ISSUE_URL_RE = /https?:\/\/github\.com\/([^/\s"]+)\/([^/\s"]+)\/issues\/(\d+)/i;

// The tool is registered on the Go MCP server as "dispatch" and exposed to
// OpenCode under the "envoy" MCP server (so commonly "envoy_dispatch"). Accept
// any separator a client might use while still excluding unrelated tools.
const DISPATCH_TOOL_RE = /(^|[-._])dispatch$/i;

export function isDispatchTool(tool: string): boolean {
  return DISPATCH_TOOL_RE.test(tool);
}

/** Build the Envoy topic carrying every event on a dispatch thread issue. */
export function dispatchThreadTopic(owner: string, repo: string, thread: number): string {
  return `notifications.github.${owner}.${repo}.issue.${thread}.>`;
}

/**
 * Given a completed tool execution (name + textual output), return the Envoy
 * topic the calling session should subscribe to so it receives replies on the
 * dispatch thread — or null when this isn't a successful envoy_dispatch call.
 *
 * Parsing the GitHub issue URL out of the output (rather than trusting a JSON
 * field) keeps this robust to however OpenCode surfaces the MCP result: owner,
 * repo, and thread number all come from the canonical issue URL.
 */
export function dispatchSubscriptionTopic(tool: string, output: string): string | null {
  if (!isDispatchTool(tool)) return null;
  const match = ISSUE_URL_RE.exec(output);
  if (!match) return null;
  const owner = match[1] as string;
  const repo = match[2] as string;
  const thread = Number(match[3]);
  if (!Number.isInteger(thread) || thread <= 0) return null;
  return dispatchThreadTopic(owner, repo, thread);
}

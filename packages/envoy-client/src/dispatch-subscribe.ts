// Auto-subscription wiring for the native dispatch tool, shared by the Envoy
// adapters (OpenCode plugin tool.execute.after hook, OMP extension
// tool_result hook).
//
// When an agent opens or continues a Dispatch thread, the human answers by
// commenting on the GitHub issue. For the agent to RECEIVE that answer, its
// session must be subscribed to the thread's Envoy topic
// (notifications.github.<owner>.<repo>.issue.<thread>.>). The dispatch
// service is stateless and has no session context, so each adapter closes the
// loop from its own host hook.
//
// This module is the pure, testable core: it turns a completed tool execution
// into the topic the calling session should subscribe to, or null when the
// execution isn't a successful dispatch call.

import { DISPATCH_TOOL_NAME } from "./dispatch-contract";

// Matches a GitHub issue URL anywhere in the tool output and captures
// owner / repo / number. The dispatch service returns {"thread":N,"url":"…"}
// as its text content, so the URL is always present on success.
const ISSUE_URL_RE = /https?:\/\/github\.com\/([^/\s"]+)\/([^/\s"]+)\/issues\/(\d+)/i;

/** Every host registers the tool under exactly this name. */
export function isDispatchTool(tool: string): boolean {
  return tool === DISPATCH_TOOL_NAME;
}

/** Build the Envoy topic carrying every event on a dispatch thread issue. */
export function dispatchThreadTopic(owner: string, repo: string, thread: number): string {
  return `notifications.github.${owner}.${repo}.issue.${thread}.>`;
}

/**
 * Given a completed tool execution (name + textual output), return the Envoy
 * topic the calling session should subscribe to so it receives replies on the
 * dispatch thread — or null when this isn't a successful dispatch call.
 *
 * Parsing the GitHub issue URL out of the output (rather than trusting a JSON
 * field) keeps this robust to however a host surfaces the result: owner,
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

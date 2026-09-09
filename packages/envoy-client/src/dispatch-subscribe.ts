/**
 * Extract the Dispatch subscription topic from a native tool result.
 * Hosts call this after any tool result and subscribe only when Dispatch
 * explicitly returned one of its issue topics.
 */
export function dispatchSubscriptionTopic(details: unknown): string | null {
  if (typeof details !== "object" || details === null || !("topic" in details)) return null;
  const { topic } = details;
  return typeof topic === "string" && topic.startsWith("notifications.dispatch.") ? topic : null;
}

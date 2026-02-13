import type { Event } from "@opencode-ai/sdk";

type NotifyCallback = (taskSessionID: string, status: string) => void;

export function createBackgroundNotificationHook(onNotify?: NotifyCallback) {
  return function backgroundNotificationHook(input: { event: Event }): void {
    const { event } = input;
    if (event.type !== "session.status") return;

    const props = event.properties as Record<string, unknown> | undefined;
    if (!props) return;

    const sessionID = props.sessionID as string | undefined;
    const status = props.status as { type: string } | undefined;

    if (!sessionID || !status) return;

    if (status.type === "idle" || status.type === "error") {
      onNotify?.(sessionID, status.type);
    }
  };
}

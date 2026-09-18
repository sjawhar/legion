import type { Comment } from "../api/types";

/** The server serializes empty mention and delivery arrays on every comment. */
export function commentDeliveryFields(): Pick<Comment, "mentions" | "deliveries"> {
  return { mentions: [], deliveries: [] };
}

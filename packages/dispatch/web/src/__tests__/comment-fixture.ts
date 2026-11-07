import type { Comment } from "../api/types";

/**
 * The fields the server writes on every comment that a fixture has no value of its own for: the
 * empty mention and delivery arrays it serializes on each one, and the three a comment carries
 * only when it is something more than a comment on an issue - the comment a reply answers
 * (`answers`), the block of it (`reply_block`), and the project document a comment of a document
 * with no issue belongs to (`artifact_id`, which the comment's own anchor does not fill).
 *
 * Spread it over a fixture, not under it, when the fixture sets one of them itself.
 */
export function commentDeliveryFields(): Pick<
  Comment,
  "answers" | "artifact_id" | "deliveries" | "mentions" | "reply_block"
> {
  return { answers: null, artifact_id: null, deliveries: [], mentions: [], reply_block: null };
}

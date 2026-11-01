import type { AskUrgency } from "../../api/types";

/** The human label of each ask urgency, shared by the Inbox card, the composer's urgency picker,
 * and a document's decision blocks. */
export const URGENCY_LABELS: Record<AskUrgency, string> = {
  blocking: "Blocking",
  high: "High",
  low: "Low",
  med: "Medium",
};

/** Urgencies in ascending order, for a picker. */
export const ASK_URGENCIES_ASCENDING = ["low", "med", "high", "blocking"] as const;

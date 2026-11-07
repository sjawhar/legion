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

/** `value` as an urgency, or `med` where this dashboard knows no such level: an urgency reaches
 *  the browser as the string the route table declares, and `ASK_URGENCIES_ASCENDING` is the closed
 *  list that string is one of. Every surface that draws an urgency - its label, its accent, its
 *  notch - narrows here, so none of them has to say what an unknown word looks like. */
export function askUrgency(value: string): AskUrgency {
  return ASK_URGENCIES_ASCENDING.find((known) => known === value) ?? "med";
}

import type { CheckBlock, CheckResult } from "../../api/types";
import type { NodeFact } from "./issue-tree";
import { CHECK_WHEN_WORDS, RESULT_CURRENCY_WORDS, RESULT_VERDICT_WORDS } from "./page-words";

/**
 * What a check and a recorded result hold that their rows have no line for, as the facts their
 * bodies draw (dispatch://INFRA-1196 R15, `dispatch://INFRA-1196/artifact/plan-md#b-t16`).
 *
 * Pure, over the results read alone, so the body of a check is the same wherever the tree draws
 * it. Two rules decide what is here:
 *
 * - **A check draws all four of its attributes, blank or not.** That a check names no negative
 *   control is itself what a reader came to see, so the fact keeps its name with no value.
 * - **A result draws only what it recorded.** `Failed 0`, `Skipped 0` and `Excluded 0` took
 *   three rows under every pass on the old tab
 *   (`dispatch://INFRA-1196/artifact/design-notes-md` section 5, fault 9), so a count is drawn
 *   when it counts something, and a currency Dispatch could not judge is drawn not at all.
 *
 * A `when` reads as words, a `run` as a command with its own copy control, and a screenshot as
 * a picture through the bytes route the page already serves.
 */

/** The four attributes of a check, in the order its body draws them. */
export function checkFacts(check: CheckBlock): NodeFact[] {
  const when: Readonly<Record<string, string>> = CHECK_WHEN_WORDS;
  return [
    { draw: "word", name: "when", value: when[check.when] ?? "" },
    { draw: "text", name: "surface", value: check.surface },
    { draw: "command", name: "run", value: check.run },
    { draw: "text", name: "negative_control", value: check.negative_control },
  ];
}

/** What one recorded result says of itself: its verdict, whether it still speaks for the work,
 *  who ran it where and how, the commit it ran at, what it counted, and its screenshots. The
 *  result's own `observed` is the node's text and is no fact of its own. */
export function resultFacts(result: CheckResult): NodeFact[] {
  const currency: Readonly<Record<string, string>> = RESULT_CURRENCY_WORDS;
  const stands = currency[result.currency ?? ""];
  const facts: NodeFact[] = [
    { draw: "word", name: "verdict", value: RESULT_VERDICT_WORDS[result.verdict] },
  ];
  if (stands !== undefined) facts.push({ draw: "word", name: "stands", value: stands });
  for (const [name, value] of [
    ["role", result.role],
    ["surface", result.surface],
  ] as const) {
    if (value !== "") facts.push({ draw: "text", name, value });
  }
  if (result.run !== "") facts.push({ draw: "command", name: "run", value: result.run });
  for (const [name, value] of [
    ["commit", result.commit_sha],
    ["negative_control", result.negative_control],
  ] as const) {
    if (value !== "") facts.push({ draw: "text", name, value });
  }
  for (const [name, count] of [
    ["failed", result.failed],
    ["skipped", result.skipped],
    ["excluded", result.excluded],
  ] as const) {
    if (count > 0) facts.push({ draw: "text", name, value: String(count) });
  }
  for (const screenshot of result.screenshots) {
    facts.push({ draw: "picture", name: "screenshot", value: screenshot });
  }
  return facts;
}

import type { AskAnswer, Suggestions, WriteAdvice, WriteSuggestion } from "@legion/contracts";
import { messageFor } from "./errors";

/** What a not-made line needs to name the search to run by hand. */
export interface RelatedCheckContext {
  /** The project the check searched, for `dispatch search --project`. */
  readonly project: string;
  /** The text the check read (a title, a question), the source of the search words when the
   *  server sent no `search_query`; empty when there is none. */
  readonly text: string;
  /** The Dispatch URL item links are made absolute against. */
  readonly configUrl: string;
}

/**
 * The related-item check's lines (INFRA-568). They hold exactly one of three forms, so a check
 * that never ran cannot read as one that found nothing: the items it found and how it searched,
 * that it searched and found nothing, or that no check was made, why, and the `dispatch search` to
 * run instead. A past decision and the titles a forced create went past follow the form.
 *
 * A server predating the forms sends less, and each of its shapes maps to one form: no result at
 * all, `missing`, items with no `outcome`, and nothing at all with no `outcome`.
 *
 * It never throws: it rides on a write that already succeeded, so a result shaped as this client
 * does not expect still prints a form rather than costing the reader the write's own line.
 */
export function relatedCheckLines(
  suggestions: Suggestions | null | undefined,
  context: RelatedCheckContext
): string[] {
  if (suggestions === null || suggestions === undefined) {
    return [notMadeLine("the server sent no check result", context)];
  }
  let lines: string[];
  try {
    lines = relatedCheckForm(suggestions, context);
  } catch (error) {
    lines = [
      notMadeLine(`this client could not read the check result (${messageFor(error)})`, context),
    ];
  }
  try {
    const { decision } = suggestions;
    if (decision !== undefined && decision !== null) {
      const when = decision.answered_at === undefined ? "" : ` on ${decision.answered_at}`;
      lines.push(
        `A past decision may already answer this: ${suggestionLabel(decision)}, answered by ${decision.answered_by}${when} → ${absoluteHref(decision.href, context.configUrl)}`
      );
    }
    for (const match of suggestions.title_matches ?? []) {
      lines.push(
        `Title overlaps you forced past: ${match.key} [${match.status}] ${match.title} → ${absoluteHref(match.href, context.configUrl)}`
      );
    }
  } catch (error) {
    lines.push(
      `The check's past decision or title overlaps could not be shown: ${messageFor(error)}`
    );
  }
  return lines;
}

/**
 * A document edit's lines about the decision blocks it added (INFRA-568): `Decision block <id>:`
 * and that block's check form, in document order, each searched by its own question. A server
 * predating the checks only counts the new blocks, so the edit gets one not-made line for all of
 * them, searched by `context.text`. An edit that added none, or a server predating even the
 * count, prints nothing. Never throws, for the reason `relatedCheckLines` gives.
 */
export function decisionBlockLines(
  advice: WriteAdvice | undefined,
  context: RelatedCheckContext
): string[] {
  try {
    const checks = advice?.decision_block_checks;
    if (checks === undefined || checks === null) {
      const added = advice?.decision_blocks_added ?? 0;
      if (added === 0) return [];
      return [
        `${added} new decision ${added === 1 ? "block" : "blocks"}:`,
        notMadeLine("the server sent no check result", context),
      ];
    }
    return checks.flatMap((check) => [
      `Decision block ${check.block_id}:`,
      ...relatedCheckLines(check.suggestions, { ...context, text: check.question ?? "" }),
    ]);
  } catch (error) {
    return [
      notMadeLine(
        `this client could not read the decision blocks' checks (${messageFor(error)})`,
        context
      ),
    ];
  }
}

function relatedCheckForm(suggestions: Suggestions, context: RelatedCheckContext): string[] {
  const { outcome, missing } = suggestions;
  if (outcome === "not_made" || (outcome === undefined && missing !== undefined)) {
    return [notMadeLine(missing ?? "the server gave no reason", context, suggestions.search_query)];
  }
  const related = suggestions.related ?? [];
  const mode = searchModeWords(suggestions);
  if (related.length > 0) {
    // A server predating the forms names no search mode, so its header names none either.
    const header = mode === undefined ? "Related items:" : `Related items (searched by ${mode}):`;
    return [header, ...related.flatMap((item) => relatedItemLines(item, context.configUrl))];
  }
  if (outcome === undefined) {
    return [
      notMadeLine("the server did not say whether it searched", context, suggestions.search_query),
    ];
  }
  if (outcome === "found" || outcome === "none") {
    return [
      mode === undefined
        ? "Related-item check searched and found nothing."
        : `Related-item check searched (${mode}) and found nothing.`,
    ];
  }
  return [
    notMadeLine(
      `the server answered an outcome this client does not know (${String(outcome)})`,
      context,
      suggestions.search_query
    ),
  ];
}

/** How the check searched, as the forms word it after "searched" or "searched by". */
function searchModeWords(suggestions: Suggestions): string | undefined {
  const { search, search_note: note } = suggestions;
  if (search === undefined) return undefined;
  if (search === "meaning_and_keywords") return "meaning and keywords";
  const words = search === "keywords_only" ? "keywords only" : String(search);
  return note === undefined || note === "" ? words : `${words}: ${note}`;
}

function relatedItemLines(item: WriteSuggestion, configUrl: string): string[] {
  try {
    const lines = [`- ${suggestionLabel(item)} → ${absoluteHref(item.href, configUrl)}`];
    if (item.kind === "ask" && item.ask !== undefined && item.ask !== null) {
      const { ask } = item;
      if (ask.state === "open") {
        lines.push("  Ask: open");
      } else if (ask.state === "answered") {
        lines.push(
          `  Ask: answered by ${answerSummary(ask.answer)}`,
          ...answerTextLines(ask.answer)
        );
      } else if (ask.answer === undefined || ask.answer === null) {
        lines.push(`  Ask: closed with no answer: ${ask.resolution.kind}`);
      } else {
        lines.push(
          `  Ask: closed after an answer: ${ask.resolution.kind}; last answered by ${answerSummary(ask.answer)}`,
          ...answerTextLines(ask.answer)
        );
      }
    }
    return lines;
  } catch (error) {
    // One item the client cannot read costs only that item, never the form.
    const href = (item as { readonly href?: unknown } | null)?.href;
    return [`- ${String(href)} (this client could not show it: ${messageFor(error)})`];
  }
}

/** `<who> at <time>, chose "<a>", "<b>"`, or `chose no option`. */
function answerSummary(answer: AskAnswer): string {
  const selected = answer.selected ?? [];
  const chose =
    selected.length === 0
      ? "chose no option"
      : `chose ${selected.map((label) => JSON.stringify(label)).join(", ")}`;
  return `${answer.user} at ${answer.at}, ${chose}`;
}

/** The answer's text under its summary, each later line indented under the first. */
function answerTextLines(answer: AskAnswer): string[] {
  const text = answer.text?.trim() ?? "";
  if (text === "") return ["  Answer text: none"];
  return [`  Answer text: ${text.split("\n").join("\n    ")}`];
}

function suggestionLabel(item: WriteSuggestion): string {
  if (item.owner.kind === "issue") {
    return `${item.owner.key} [${item.owner.status}] ${item.owner.title}`;
  }
  return `${item.owner.name} (${item.owner.project}/${item.owner.slug})`;
}

/** The link as an absolute URL, or as the server sent it when it does not parse as one. */
function absoluteHref(href: string, configUrl: string): string {
  try {
    return new URL(href, configUrl).toString();
  } catch {
    return String(href);
  }
}

/**
 * `No related-item check was made: <reason>.` and the search to run by hand: `searchQuery` when
 * the server chose the words, else the first three words of four or more letters in the text the
 * check read. With no such word the command names what to put in `--query` instead of guessing.
 */
export function notMadeLine(
  reason: string,
  context: RelatedCheckContext,
  searchQuery?: string
): string {
  const words = searchQuery?.trim() || searchWords(context.text);
  const query =
    words === ""
      ? " with two or three of its words as --query"
      : ` --query '${words.replaceAll("'", "'\\''")}'`;
  return `No related-item check was made: ${reason}. Search before you rely on this being new: dispatch search --project ${context.project}${query}`;
}

/** The first three words of four or more letters: short words are mostly ones every item holds. */
function searchWords(text: string): string {
  return (text.match(/[\p{L}\p{N}_]+/gu) ?? [])
    .filter((word) => (word.match(/\p{L}/gu) ?? []).length >= 4)
    .slice(0, 3)
    .join(" ");
}

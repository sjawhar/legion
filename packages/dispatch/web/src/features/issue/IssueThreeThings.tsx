import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { artifactBlocksQuery } from "../../api/queries";
import type { Artifact, ArtifactBlock } from "../../api/types";
import {
  borderDefault,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { liveTextQueryOptions } from "../doc/ProofDocument";
import { presetPath } from "./issue-address";
import type { TreeNode } from "./issue-tree";
import { NodeGlyph } from "./NodeGlyph";
import { authorText, pageChrome } from "./page-structure";
import {
  APPROVAL_STATE_WORDS,
  type CheckState,
  CONTROL_LABELS,
  EMPTY_STATE_LINES,
  GAP_LINES,
  GAP_WORDS,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
  PRESET_NAMES,
} from "./page-words";

/**
 * The head of the page: the issue's three sentences, and the two rows under them
 * (dispatch://INFRA-1196 R5, `dispatch://INFRA-1196/artifact/plan-md#b-t10`).
 *
 * It is drawn above the presets and on all five, because what the issue is does not change
 * with the query a reader is running over it.
 *
 * The sentences are the spec's summary, as the spec stores them: the rendered text equals the
 * stored text, split at its sentence ends and at nothing else. It is read from the document's
 * own text at the summary block's own span, never from the block's stored excerpt, which the
 * server cuts at 240 runes (`refs/graph.go`) and which would therefore make a longer summary
 * read as a shorter one. The two reads it takes are the two the page already makes - the
 * document's text, which its editor reads, and the document's blocks, which the tree reads -
 * so the head costs no read of its own, and the excerpt stands in only while the text has not
 * answered yet. Who wrote the bullets and when they were last read back are
 * dispatch://INFRA-1278 and the model holds neither, so the head says so in two gaps and draws
 * no author at all - it never guesses one from the version the summary last changed in.
 *
 * The two rows are the page's own words and hold no author's text, so the sentences above them
 * are the only thing a reader of the head quotes. Each names one source and no second: the
 * Plan preset with how the plan stands with its reviewers, and the Checks preset with the
 * state every criterion's latest result is in. The count behind each of those states is in the
 * preset itself, one row per criterion, which is one press away.
 */
export function IssueThreeThings({
  checks,
  issueKey,
  plan,
  spec,
  planTasks,
  summary,
}: {
  /** Every check of the issue, as the tree holds them: each one's state is its latest result. */
  checks: readonly TreeNode[];
  issueKey: string;
  /** The issue's plan document, where it has one. */
  plan: Artifact | undefined;
  /** The issue's spec, whose text the summary's own words are read out of. */
  spec: Artifact | undefined;
  /** How many plan tasks the Plan preset holds. */
  planTasks: number;
  /** The spec's summary node, or undefined for a spec that carries none. */
  summary: TreeNode | undefined;
}): ReactNode {
  const specId = spec?.id ?? "";
  const blocks = useQuery({ ...artifactBlocksQuery(specId), enabled: specId !== "" });
  const text = useQuery({ ...liveTextQueryOptions(specId), enabled: specId !== "" });
  const sentences = sentencesOf(
    storedText(text.data?.markdown, blocks.data, summary?.id) ?? summary?.text ?? ""
  );
  const states = Object.keys(NODE_STATE_WORDS.check).filter((state) =>
    checks.some((check) => check.state === state)
  ) as CheckState[];
  const approval = plan?.approval?.state;
  return (
    <section
      className={`mb-3 flex min-w-0 flex-col gap-2 border-b pb-3 ${borderDefault}`}
      data-testid="issue-three-things"
    >
      {sentences.length === 0 ? (
        <p className={`text-sm ${textMutedOnCanvas}`} {...pageChrome}>
          {EMPTY_STATE_LINES.spec_summary}
        </p>
      ) : (
        <ul className="flex min-w-0 flex-col gap-1">
          {sentences.map((sentence) => (
            <li
              className={`text-sm break-words ${textSecondaryOnCanvas}`}
              key={sentence}
              {...authorText}
            >
              {sentence}
            </li>
          ))}
        </ul>
      )}
      <p className={`flex min-w-0 flex-wrap items-baseline gap-2 text-xs ${textMutedOnCanvas}`}>
        {(["no_summary_author", "no_read_back"] as const).map((gap) => (
          <span
            className={`rounded border px-1 ${borderDefault}`}
            data-gap=""
            data-gap-kind={gap}
            key={gap}
            title={undefined}
            {...pageChrome}
          >
            {GAP_WORDS[gap]}
          </span>
        ))}
        <span {...pageChrome}>
          {GAP_LINES.no_summary_author} {GAP_LINES.no_read_back}
        </span>
      </p>
      <p
        className={`flex min-w-0 flex-wrap items-baseline gap-2 text-sm ${textMutedOnCanvas}`}
        data-testid="issue-plan-row"
      >
        <span {...pageChrome}>{CONTROL_LABELS.how_it_gets_done}</span>
        <Link
          className={`underline ${linkText} ${linkHoverText}`}
          to={presetPath(issueKey, "plan")}
        >
          <span {...pageChrome}>{PRESET_NAMES.plan}</span>
        </Link>
        {planTasks === 0 ? (
          <span {...pageChrome}>{EMPTY_STATE_LINES.plan}</span>
        ) : approval === undefined ? null : (
          <span {...pageChrome}>{APPROVAL_STATE_WORDS[approval]}</span>
        )}
      </p>
      <p
        className={`flex min-w-0 flex-wrap items-baseline gap-2 text-sm ${textMutedOnCanvas}`}
        data-testid="issue-checks-row"
      >
        <span {...pageChrome}>{CONTROL_LABELS.what_you_will_be_shown}</span>
        <Link
          className={`underline ${linkText} ${linkHoverText}`}
          to={presetPath(issueKey, "checks")}
        >
          <span {...pageChrome}>{PRESET_NAMES.checks}</span>
        </Link>
        {states.length === 0 ? (
          <span {...pageChrome}>{EMPTY_STATE_LINES.checks}</span>
        ) : (
          states.map((state) => (
            <span className="flex items-center gap-1" key={state} {...pageChrome}>
              <NodeGlyph token={GLYPH_TOKENS.states.check[state]} />
              <span>{NODE_STATE_WORDS.check[state]}</span>
            </span>
          ))
        )}
      </p>
    </section>
  );
}

/**
 * The block's own stored text, cut out of the document's markdown at the span the blocks read
 * gives it (`ArtifactBlock.from`/`to`, which the server slices the same markdown with -
 * `docs/closed_version_blocks.go`). The span is in bytes, so the cut is made over the encoded
 * bytes rather than over JavaScript's UTF-16 units, and a summary holding anything outside
 * ASCII still reads as it was written. `undefined` while either read is still out, or for a
 * block the document no longer holds.
 */
function storedText(
  markdown: string | undefined,
  blocks: readonly ArtifactBlock[] | undefined,
  nodeId: string | undefined
): string | undefined {
  const blockId = nodeId?.split("#b-")[1];
  const block = blocks?.find((candidate) => candidate.id === blockId);
  if (markdown === undefined || block === undefined) return undefined;
  const bytes = new TextEncoder().encode(markdown);
  return new TextDecoder().decode(bytes.slice(block.from, block.to));
}

/**
 * The summary's own sentences, split at their sentence ends: each one as it was written, with
 * nothing added, removed or reflowed, so what the head draws is what the spec holds.
 *
 * A typed block's stored text is its source, so it arrives inside its own `:::` fence. The
 * fence is the document's syntax and no part of what anybody wrote, so it is dropped before
 * the split.
 */
function sentencesOf(text: string): readonly string[] {
  const lines = text.split("\n");
  if (lines[0]?.startsWith(":::") === true) lines.shift();
  if (lines.at(-1)?.trim() === ":::") lines.pop();
  return lines
    .join(" ")
    .replace(/\s+/gu, " ")
    .trim()
    .split(/(?<=[.!?])\s+/u)
    .filter((sentence) => sentence !== "");
}

/** The issue's summary: the first `callout` block of its spec, which is the nearest thing
 *  today's block schema holds to a summary node (dispatch://INFRA-1278 adds one). */
export function specSummary(root: TreeNode, specSlug: string): TreeNode | undefined {
  const prefix = `${specSlug}#b-`;
  const walk = (node: TreeNode): TreeNode | undefined => {
    if (node.type === "callout" && node.id.startsWith(prefix)) return node;
    for (const child of node.children) {
      const found = walk(child);
      if (found !== undefined) return found;
    }
    return undefined;
  };
  return specSlug === "" ? undefined : walk(root);
}

/** Every node of `type` the tree holds, in reading order. */
export function nodesOfType(root: TreeNode, type: TreeNode["type"]): TreeNode[] {
  const found: TreeNode[] = [];
  const walk = (node: TreeNode): void => {
    if (node.type === type) found.push(node);
    for (const child of node.children) walk(child);
  };
  walk(root);
  return found;
}

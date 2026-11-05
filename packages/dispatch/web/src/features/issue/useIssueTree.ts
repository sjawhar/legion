import { useQueries, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";
import {
  artifactBlocksQuery,
  issueCoverageQuery,
  issueResultsQuery,
  referenceTargetsQuery,
} from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import {
  buildIssueTree,
  type DocumentBlocks,
  type IssueTreeReads,
  type TreeNode,
  treeReferences,
} from "./issue-tree";

/**
 * The issue's tree, from the page's reads (dispatch://INFRA-1196 R20). This hook is where the
 * reads are; `buildIssueTree` is pure and holds none of them, so the model is testable without a
 * server and the page makes no request per node.
 *
 * The reads are the issue's own (handed in, because the page already has it), the coverage, the
 * decisions, the results, the references, the issue's asks, and the blocks of each document -
 * ids, types and attributes, never text, which is how a `serves` edge is held without reading a
 * document the reader has not opened. Every one of them is a key another surface of this issue
 * already reads, so opening the Conversation or the Decisions panel after the tree finds them
 * answered rather than fetching again. The conversation's comments and messages are nodes of
 * this tree too, and a child issue's check reads them (`tree-drafts.ts`, `treeDrafts`).
 *
 * The names come last and all at once: the tree is built, every address it holds is read in one
 * batched request (`referenceTargetsQuery`), and the tree is built again with the names in it.
 * Until that answers the tree draws, with a chip naming its target's address and a block of a
 * document carrying no words yet.
 */
export function useIssueTree(issue: IssueDetails): TreeNode {
  const key = issue.key;
  const coverage = useQuery(issueCoverageQuery(key));
  const decisions = useQuery(issueDecisionsQuery(key, true));
  const results = useQuery(issueResultsQuery(key));
  const references = useQuery({
    queryKey: ["issue", key, "references"],
    queryFn: () => api.getIssueReferences(key),
  });
  const asks = useQuery({
    queryKey: ["asks", key],
    queryFn: () => api.listIssueAsks(key, "all"),
  });
  // Under the keys the Conversation and the margin already own, so a reader opening the panel
  // after the tree finds them answered rather than read a second time.
  const comments = useQuery({
    queryKey: ["comments", key],
    queryFn: () => api.listComments(key),
  });
  const messages = useQuery({
    queryKey: ["issue-messages", key],
    queryFn: () => api.listIssueMessages(key),
  });
  const documentIds = issue.artifacts
    .filter((artifact) => artifact.kind === "doc")
    .map((artifact) => artifact.id);
  // `combine` is what keeps one value for every document's blocks: TanStack holds its answer and
  // replaces it only when a document's own read changes, where a map over the results would be a
  // new array every render and build the tree again with it.
  const documents = useQueries({
    combine: (answers): DocumentBlocks[] =>
      answers.map((answer, index) => ({
        artifactId: documentIds[index] ?? "",
        blocks: answer.data ?? [],
      })),
    queries: documentIds.map((id) => artifactBlocksQuery(id)),
  });
  // One value, so the model is given the reads and nothing else.
  const reads = useMemo<IssueTreeReads>(
    () => ({
      asks: asks.data,
      comments: comments.data,
      coverage: coverage.data,
      decisions: decisions.data,
      documents,
      issue,
      messages: messages.data,
      references: references.data,
      results: results.data,
    }),
    [
      asks.data,
      comments.data,
      coverage.data,
      decisions.data,
      documents,
      issue,
      messages.data,
      references.data,
      results.data,
    ]
  );
  const unnamed = useMemo(() => buildIssueTree(reads), [reads]);
  const refs = useMemo(() => treeReferences(unnamed), [unnamed]);
  const targets = useQuery(referenceTargetsQuery(refs));
  const named = targets.data?.targets;
  return useMemo(
    () => (named === undefined ? unnamed : buildIssueTree({ ...reads, targets: named })),
    [named, reads, unnamed]
  );
}

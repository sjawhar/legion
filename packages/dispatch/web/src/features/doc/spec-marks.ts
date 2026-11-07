import type { BlockMark } from "@legion/proof-editor";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import { api, documentReadRequest } from "../../api/client";
import type { Artifact, ArtifactBlock, GraphReferences, IssueDecisions } from "../../api/types";
import { changedSinceReadMark, staleBlockMark } from "../../theme/classes";
import { useSendMarkOnce } from "../writes/useSendMarkOnce";

/** What a stale block says when the reader rests on it. */
export const STALE_BLOCK_TITLE = "A decision that shaped this block changed after it.";

/** One block of the spec as its `part_of` edge reads it: the version in which its text last
 *  changed (none where the server never recorded one) and the head of its text. */
export interface SpecBlockPart {
  readonly blockId: string;
  readonly changedVersion: number | null;
  readonly text: string;
}

/** The blocks that changed since the version the person last read, in document order, and the
 *  heading of each one's section, each once. */
export interface ChangedSinceRead {
  readonly blockIds: readonly string[];
  readonly headings: readonly string[];
  readonly version: number;
}

/** The decisions view, which the margin's decision cards read under the same key. */
export const issueDecisionsQuery = (issueKey: string, enabled: boolean) => ({
  enabled,
  queryFn: () => api.getIssueDecisions(issueKey),
  queryKey: ["decisions", issueKey] as const,
});

/** Every spec block a stale link of any decision names, each once, in the order the view names
 *  them. A link is stale when its decision changed after it first linked the block and the block
 *  has not changed since. */
export function staleSpecBlocks(decisions: IssueDecisions | undefined): string[] {
  const stale: string[] = [];
  for (const decision of decisions?.decisions ?? []) {
    for (const link of decision.links ?? []) {
      const blockId = link.to.block_id;
      if (link.stale && blockId !== undefined && !stale.includes(blockId)) stale.push(blockId);
    }
  }
  return stale;
}

/** The spec's blocks from a read of its `part_of` edges. */
export function specBlockParts(references: GraphReferences): SpecBlockPart[] {
  return (references.edges ?? []).flatMap((edge) => {
    const blockId = edge.node.block_id;
    if (edge.kind !== "part_of" || edge.node.kind !== "block" || blockId === undefined) return [];
    return [
      {
        blockId,
        changedVersion: edge.node.changed_version ?? null,
        text: edge.excerpt?.text ?? "",
      },
    ];
  });
}

/**
 * The blocks whose text changed in a version above `readVersion`, read from the spec's blocks in
 * document order (`blocks`, the listing, by position) and their parts. Each is named by the
 * heading of the section it sits in - itself, for a heading - or, before the first heading, by
 * its own first line. Undefined when the person has read no version, or nothing changed since.
 */
export function changedSinceRead(
  blocks: readonly ArtifactBlock[],
  parts: readonly SpecBlockPart[],
  readVersion: number | null
): ChangedSinceRead | undefined {
  if (readVersion === null) return undefined;
  const partOf = new Map(parts.map((part) => [part.blockId, part]));
  const blockIds: string[] = [];
  const headings: string[] = [];
  let section: string | undefined;
  for (const block of [...blocks].sort((left, right) => left.from - right.from)) {
    const part = partOf.get(block.id);
    // A heading's words: its markdown without the leading `#`s.
    if (block.type === "heading") section = (part?.text ?? "").replace(/^#{1,6}\s+/u, "").trim();
    if (part === undefined || part.changedVersion === null || part.changedVersion <= readVersion) {
      continue;
    }
    blockIds.push(block.id);
    const name = section ?? part.text.split("\n", 1)[0]?.trim() ?? "";
    if (!headings.includes(name)) headings.push(name);
  }
  return blockIds.length === 0 ? undefined : { blockIds, headings, version: readVersion };
}

/** The one line above the document. */
export function readLineText(changed: ChangedSinceRead): string {
  return `Changed since you last read version ${changed.version}: ${changed.headings.join(", ")}`;
}

/** The editor's block marks: each stale block with its title, then each changed block. */
export function specBlockMarks(stale: readonly string[], changed: readonly string[]): BlockMark[] {
  return [
    ...stale.map((blockId) => ({ blockId, className: staleBlockMark, title: STALE_BLOCK_TITLE })),
    ...changed.map((blockId) => ({ blockId, className: changedSinceReadMark })),
  ];
}

const NO_MARKS: readonly BlockMark[] = [];

/**
 * The Spec tab's marks for one visit: it reads the issue's decisions and the person's last read
 * version of the spec, marks each block a stale link names, and - when the person has read a
 * version - each block that changed since it, with the line naming their sections. Then it records
 * `shownVersion` as read. The read version is the one the visit opened on, so the marks stay for
 * the visit after the record moves it.
 */
export function useSpecMarks({
  artifact,
  enabled,
  issueKey,
  shownVersion,
}: {
  artifact: Artifact;
  /** The live spec of an issue, on its own page. */
  enabled: boolean;
  issueKey: string;
  /** The version the page shows: the spec's latest. */
  shownVersion: number | undefined;
}): { changed: ChangedSinceRead | undefined; marks: readonly BlockMark[] } {
  const queryClient = useQueryClient();
  const decisions = useQuery(issueDecisionsQuery(issueKey, enabled));
  const readKey = ["document-read", artifact.id] as const;
  const read = useQuery({
    enabled,
    queryFn: () => api.getDocumentRead(artifact.id),
    queryKey: readKey,
  });
  // The read version this visit opened on, set during render from the first answer (React's
  // pattern for state derived from props) so the render that has it already marks the blocks.
  const [opened, setOpened] = useState<{ artifactId: string; version: number | null }>();
  const readAtOpen = opened?.artifactId === artifact.id ? opened : undefined;
  if (enabled && readAtOpen === undefined && read.data !== undefined) {
    setOpened({ artifactId: artifact.id, version: read.data.version });
  }
  const readVersion = readAtOpen?.version ?? null;
  const specReference = `dispatch://${issueKey}/spec`;
  const parts = useQuery({
    enabled: enabled && readVersion !== null,
    queryFn: () => api.getReferences(specReference, { kind: "part_of" }),
    queryKey: ["references", "to", specReference, "part_of"],
  });
  const blocks = useQuery({
    enabled: enabled && readVersion !== null,
    queryFn: () => api.getArtifactBlocks(artifact.id),
    queryKey: ["artifact", artifact.id, "blocks"],
  });
  const changed = useMemo(
    () =>
      parts.data === undefined || blocks.data === undefined
        ? undefined
        : changedSinceRead(blocks.data, specBlockParts(parts.data), readVersion),
    [blocks.data, parts.data, readVersion]
  );
  const marks = useMemo(() => {
    if (!enabled) return NO_MARKS;
    const stale = staleSpecBlocks(decisions.data);
    const changedIds = changed?.blockIds ?? [];
    return stale.length === 0 && changedIds.length === 0
      ? NO_MARKS
      : specBlockMarks(stale, changedIds);
  }, [changed, decisions.data, enabled]);
  useSendMarkOnce({
    mark:
      enabled && readAtOpen !== undefined && shownVersion !== undefined
        ? { key: `${artifact.id}@${shownVersion}`, value: shownVersion }
        : undefined,
    onSuccess: (result) => queryClient.setQueryData(readKey, result),
    request: (version) => documentReadRequest(artifact.id, version),
    shouldSend: true,
    target: `read:document:${artifact.id}`,
  });
  return { changed: enabled ? changed : undefined, marks };
}

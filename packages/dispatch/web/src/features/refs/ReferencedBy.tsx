import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import type { GraphEdge } from "../../api/types";
import {
  borderDefault,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { blockFragment, routeBlock } from "./block-reference";
import { referenceTriggerProps } from "./RefPreview";
import { referencesQuery } from "./reference-target";
import type { DispatchReferenceRoute } from "./routes";
import { buildReferencePath, parseDispatchReference } from "./routes";
import { Timestamp } from "./Timestamp";

const NODE_LABELS: Record<GraphEdge["node"]["kind"], string> = {
  artifact: "Artifact",
  ask: "Ask",
  // A block that cites a node shows as the document holding it, linked to the block.
  block: "Artifact",
  comment: "Comment",
  component: "Component",
  issue: "Issue",
  message: "Message",
  session: "Session",
};

const STRUCTURAL_LABELS: Partial<Record<GraphEdge["kind"], string>> = {
  anchored_to: "Anchored item",
  attached_to: "Attached document",
  blocked_by: "Blocked issue",
  child_of: "Child issue",
  followed_by: "Follower",
  owned_by: "Owned item",
  replies_to: "Reply",
};

/** Beyond this the row clamps the excerpt and offers to expand it. The real clamp is CSS, so
 *  this is the text shape that makes a clamp likely rather than a measurement. */
const CLAMP_LINES = 3;
const CLAMP_CHARACTERS = 240;

function edgeLabel(edge: GraphEdge): string {
  return STRUCTURAL_LABELS[edge.kind] ?? NODE_LABELS[edge.node.kind];
}

function sourceLabel(edge: GraphEdge): string {
  const source = edge.node.issue_key ?? edge.node.project ?? edge.node.id;
  return `${edgeLabel(edge)} · ${source}`;
}

interface SourcePath {
  readonly href: string;
  readonly route: DispatchReferenceRoute;
}

/** Where `reference` opens, with `block` (the block of a document that holds the mention) as its
 *  `#b-` fragment when the reference names no block of its own. */
function referencePath(reference: string | undefined, block?: string): SourcePath | undefined {
  const route = reference === undefined ? undefined : parseDispatchReference(reference);
  if (route === undefined) {
    return undefined;
  }
  return {
    href: `${buildReferencePath(route)}${
      routeBlock(route) !== undefined || block === undefined ? "" : blockFragment(block)
    }`,
    route,
  };
}

interface SourceGroup {
  readonly edges: GraphEdge[];
  readonly key: string;
  readonly label: string;
  /** How many citing nodes the group holds: more than one only when blocks of one document cite
   *  the node, each then shown as a row linked to its own block. */
  readonly nodes: number;
  readonly path: SourcePath | undefined;
}

/** One entry per citing source, in the server's newest-first order: a document that both holds
 *  an ask's anchor and mentions it is one source with two rows, not two cards, and the blocks of
 *  one document that cite the node are that document's rows. A source of one node links to that
 *  node (a block at the block); a document whose blocks are its rows links to the document, and
 *  each row to its own block. */
function groupBySource(edges: readonly GraphEdge[]): SourceGroup[] {
  const groups = new Map<string, { edges: GraphEdge[]; nodes: Set<string> }>();
  for (const edge of edges) {
    const key = edge.node.document ?? edge.node.ref ?? `${edge.node.kind}:${edge.node.id}`;
    const group = groups.get(key) ?? { edges: [], nodes: new Set<string>() };
    group.edges.push(edge);
    group.nodes.add(`${edge.node.kind}:${edge.node.id}`);
    groups.set(key, group);
  }
  return [...groups].map(([key, { edges: grouped, nodes }]) => {
    const [first] = grouped as [GraphEdge, ...GraphEdge[]];
    return {
      edges: grouped,
      key,
      label: sourceLabel(first),
      nodes: nodes.size,
      path:
        nodes.size === 1
          ? referencePath(first.node.ref, first.excerpt?.block_id)
          : referencePath(key),
    };
  });
}

function Excerpt({ text }: { text: string }): ReactNode {
  const [expanded, setExpanded] = useState(false);
  const clampable = text.split("\n").length > CLAMP_LINES || text.length > CLAMP_CHARACTERS;

  return (
    <>
      <p
        className={`mt-1 whitespace-pre-wrap text-sm ${expanded || !clampable ? "" : "line-clamp-3"} ${textSecondaryOnSurface}`}
      >
        {text}
      </p>
      {clampable ? (
        <button
          aria-expanded={expanded}
          className={`min-h-11 text-xs font-medium ${linkText} ${linkHoverText}`}
          onClick={() => setExpanded((open) => !open)}
          type="button"
        >
          {expanded ? "Show less" : "Show more"}
        </button>
      ) : null}
    </>
  );
}

/** The control every host uses: the count comes from the list response the page already read,
 *  so a card or a header costs no graph request until the reader opens its panel. A count of
 *  zero is a fact, not an affordance: it renders as plain text, since opening it would only say
 *  nothing references the node. `controls` names the panel the host renders when expanded. */
export function ReferencedByToggle({
  controls,
  count,
  expanded,
  onToggle,
}: {
  controls: string;
  count: number;
  expanded: boolean;
  onToggle: () => void;
}): ReactNode {
  if (count === 0) {
    return (
      <span
        className={`inline-flex min-h-11 shrink-0 items-center px-2 py-1 text-sm ${textMutedOnSurface}`}
      >
        Referenced by (0)
      </span>
    );
  }

  return (
    <button
      aria-controls={controls}
      aria-expanded={expanded}
      className={`flex min-h-11 shrink-0 items-center gap-2 rounded-lg px-2 py-1 text-sm font-medium ${linkText} ${linkHoverText}`}
      onClick={onToggle}
      type="button"
    >
      Referenced by ({count})
    </button>
  );
}

/** The panel: one list of what references a node, read the first time a host renders it. Its
 *  host owns disclosure — `ReferencedByToggle` beside it, or nothing at all where the page shows
 *  the list outright — so this component has one behaviour. */
export function ReferencedBy({
  className,
  reference,
  toggle,
}: {
  className?: string;
  reference: string;
  /** The control that discloses this panel: its `id` (which the control names in
   *  `aria-controls`) and the fact that it already reads `Referenced by (N)`, so the panel
   *  carries no heading of its own under it. Absent where the page shows the list outright,
   *  and then the panel is its own heading. */
  toggle?: { id: string };
}): ReactNode {
  const query = useQuery(referencesQuery(reference));
  const groups = groupBySource(query.data?.edges ?? []);

  return (
    <section aria-label="Referenced by" className={`space-y-2 ${className ?? ""}`} id={toggle?.id}>
      {toggle === undefined ? (
        <h3 className={`text-sm font-semibold ${textSecondaryOnSurface}`}>
          Referenced by{query.data === undefined ? "" : ` (${query.data.edges.length})`}
        </h3>
      ) : null}
      {query.isPending ? (
        <p className={`text-sm ${textMutedOnSurface}`}>Loading references…</p>
      ) : null}
      {query.isError ? (
        <p className={`flex flex-wrap items-center gap-2 text-sm ${textMutedOnSurface}`}>
          <span>Referenced by unavailable.</span>
          <button
            className={`font-medium underline ${linkText} ${linkHoverText}`}
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
            type="button"
          >
            Retry
          </button>
        </p>
      ) : null}
      {query.data?.edges.length === 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`}>Nothing references this yet.</p>
      ) : null}
      {groups.length === 0 ? null : (
        <ul className="space-y-2">
          {groups.map((group) => (
            <li className={`rounded-lg border p-3 ${borderDefault}`} key={group.key}>
              <div className={`text-xs font-medium ${textMutedOnSurface}`}>
                {group.path === undefined ? (
                  <span>{group.label}</span>
                ) : (
                  <Link
                    {...referenceTriggerProps(group.path.route)}
                    className={`dispatch-inline-link underline ${linkText} ${linkHoverText}`}
                    to={group.path.href}
                  >
                    {group.label}
                  </Link>
                )}
              </div>
              <ul className="space-y-2">
                {group.edges.map((edge) => {
                  const row =
                    group.nodes === 1
                      ? undefined
                      : referencePath(edge.node.ref, edge.excerpt?.block_id);
                  return (
                    <li key={`${edge.node.kind}:${edge.node.id}:${edge.kind}:${edge.direction}`}>
                      <div
                        className={`flex flex-wrap items-center gap-x-1 text-xs ${textMutedOnSurface}`}
                      >
                        {group.edges.length === 1 ? null : (
                          <>
                            <span>{edgeLabel(edge)}</span>
                            <span aria-hidden="true">·</span>
                          </>
                        )}
                        {row === undefined ? (
                          <Timestamp at={edge.created_at} />
                        ) : (
                          <Link
                            {...referenceTriggerProps(row.route)}
                            className={`underline ${linkText} ${linkHoverText}`}
                            to={row.href}
                          >
                            <Timestamp at={edge.created_at} />
                          </Link>
                        )}
                      </div>
                      {edge.excerpt === undefined ? null : <Excerpt text={edge.excerpt.text} />}
                    </li>
                  );
                })}
              </ul>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

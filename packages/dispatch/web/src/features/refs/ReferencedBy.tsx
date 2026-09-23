import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { GraphEdge } from "../../api/types";
import {
  borderDefault,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { referenceTriggerProps } from "./RefPreview";
import type { DispatchReferenceRoute } from "./routes";
import { buildReferencePath, parseDispatchReference } from "./routes";
import { Timestamp } from "./Timestamp";

const NODE_LABELS: Record<GraphEdge["node"]["kind"], string> = {
  artifact: "Artifact",
  ask: "Ask",
  comment: "Comment",
  component: "Component",
  issue: "Issue",
  message: "Message",
  session: "Session",
};

const STRUCTURAL_LABELS: Partial<Record<GraphEdge["kind"], string>> = {
  anchored_to: "Anchored item",
  attached_to: "Attached document",
  child_of: "Child issue",
  followed_by: "Follower",
  owned_by: "Owned item",
  replies_to: "Reply",
};

/** Beyond this the row clamps the excerpt and offers to expand it. The real clamp is CSS, so
 *  this is the text shape that makes a clamp likely rather than a measurement. */
const CLAMP_LINES = 3;
const CLAMP_CHARACTERS = 240;

function referencedByQuery(reference: string) {
  return {
    queryKey: ["references", "to", reference] as const,
    queryFn: () => api.getReferences(reference),
  };
}

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

function sourcePath(edge: GraphEdge): SourcePath | undefined {
  if (edge.node.ref === undefined) {
    return undefined;
  }
  const route = parseDispatchReference(edge.node.ref);
  if (route === undefined) {
    return undefined;
  }
  return {
    href: `${buildReferencePath(route)}${
      edge.excerpt?.block_id === undefined ? "" : `#b-${encodeURIComponent(edge.excerpt.block_id)}`
    }`,
    route,
  };
}

interface SourceGroup {
  readonly edges: GraphEdge[];
  readonly key: string;
  readonly label: string;
  readonly path: SourcePath | undefined;
}

/** One entry per citing node, in the server's newest-first order: a document that both holds an
 *  ask's anchor and mentions it is one source with two rows, not two cards. */
function groupBySource(edges: readonly GraphEdge[]): SourceGroup[] {
  const groups = new Map<string, SourceGroup>();
  for (const edge of edges) {
    const key = edge.node.ref ?? `${edge.node.kind}:${edge.node.id}`;
    const group = groups.get(key);
    if (group === undefined) {
      groups.set(key, { edges: [edge], key, label: sourceLabel(edge), path: sourcePath(edge) });
      continue;
    }
    group.edges.push(edge);
  }
  return [...groups.values()];
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
 *  host owns disclosure — `ReferencedByToggle` beside it, or nothing at all where the page
 *  shows the list outright — so this component has one behaviour. A host that discloses it
 *  passes the `id` its control points at, and that control already names the panel and carries
 *  the count, so the panel does not repeat the label under it. */
export function ReferencedBy({
  className,
  id,
  reference,
}: {
  className?: string;
  id?: string;
  reference: string;
}): ReactNode {
  const query = useQuery(referencedByQuery(reference));
  const groups = groupBySource(query.data?.edges ?? []);

  return (
    <section aria-label="Referenced by" className={`space-y-2 ${className ?? ""}`} id={id}>
      {id === undefined ? (
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
                    className={`underline ${linkText} ${linkHoverText}`}
                    to={group.path.href}
                  >
                    {group.label}
                  </Link>
                )}
              </div>
              <ul className="space-y-2">
                {group.edges.map((edge) => (
                  <li key={`${edge.kind}:${edge.direction}:${edge.created_at}`}>
                    <div
                      className={`flex flex-wrap items-center gap-x-1 text-xs ${textMutedOnSurface}`}
                    >
                      {group.edges.length === 1 ? null : (
                        <>
                          <span>{edgeLabel(edge)}</span>
                          <span aria-hidden="true">·</span>
                        </>
                      )}
                      <Timestamp at={edge.created_at} />
                    </div>
                    {edge.excerpt === undefined ? null : <Excerpt text={edge.excerpt.text} />}
                  </li>
                ))}
              </ul>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

import type { GraphReferences } from "../api/types";

/** One node of the reference graph, as `GET /api/v1/references` answers with it. */
export type GraphNode = GraphReferences["node"];

/**
 * A graph node with the fields the server writes on every one of them, and whatever the caller
 * names on top. Dispatch answers `made_at`, `made_by`, `changed_at` and `changed_by` with null on
 * a node nothing has made or changed - every node but a block of a document with a recorded
 * authorship - and `parent`/`parent_version` with null on a node that is not part of another.
 */
export function graphNode(fields: Partial<GraphNode> & Pick<GraphNode, "id" | "kind">): GraphNode {
  return { ...graphNodeFields(), ...fields };
}

/**
 * The same six fields on their own, for a fixture that builds the rest of its node inline:
 * spread this first and let the fixture's own fields win.
 */
export function graphNodeFields(): Pick<
  GraphNode,
  "changed_at" | "changed_by" | "made_at" | "made_by" | "parent" | "parent_version"
> {
  return {
    changed_at: null,
    changed_by: null,
    made_at: null,
    made_by: null,
    parent: null,
    parent_version: null,
  };
}

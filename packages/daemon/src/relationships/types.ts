/**
 * Types for cross-issue relationship tracking.
 *
 * Read-only, informational only — does NOT affect the state machine
 * or scheduling decisions (suggestAction).
 */

/**
 * A directional relationship between two issues.
 */
export interface Relationship {
  parent: string;
  child: string;
  type: "parent-child";
}

/**
 * Persisted relationship graph.
 */
export interface RelationshipGraph {
  relationships: Relationship[];
}

/**
 * Empty graph constant for initialization and error recovery.
 */
export const EMPTY_GRAPH: RelationshipGraph = { relationships: [] };

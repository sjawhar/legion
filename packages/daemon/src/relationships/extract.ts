/**
 * Extract cross-issue relationships from issue body text.
 *
 * Currently supports:
 * - "Part of #NNN" → parent-child (current issue is child of #NNN)
 */

import type { Relationship } from "./types";

/**
 * Pattern: "Part of #NNN" (case-insensitive).
 * Captures the issue number.
 */
const PART_OF_PATTERN = /part\s+of\s+#(\d+)/gi;

/**
 * Extract parent-child relationships from issue body text.
 *
 * When an issue body contains "Part of #NNN", the current issue is a child
 * of issue #NNN. The issueId format must match the caller's convention
 * (e.g., "owner-repo-NNN" for GitHub).
 *
 * @param childIssueId - The issue ID of the issue whose body we're parsing
 * @param body - The issue body/description text
 * @param repoPrefix - Optional prefix for parent issue IDs (e.g., "owner-repo")
 * @returns Array of parent-child relationships
 */
export function extractRelationshipsFromBody(
  childIssueId: string,
  body: string,
  repoPrefix?: string
): Relationship[] {
  const relationships: Relationship[] = [];
  const seen = new Set<string>();

  for (const match of body.matchAll(PART_OF_PATTERN)) {
    const parentNumber = match[1];
    const parentId = repoPrefix ? `${repoPrefix}-${parentNumber}` : `#${parentNumber}`;

    // Deduplicate within a single body
    if (seen.has(parentId)) {
      continue;
    }
    seen.add(parentId);

    relationships.push({
      parent: parentId,
      child: childIssueId,
      type: "parent-child",
    });
  }

  return relationships;
}

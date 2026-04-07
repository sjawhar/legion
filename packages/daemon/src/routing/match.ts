import type { Domain, RoutingConfig } from "./schema";

/**
 * A matched domain with its name and the reviewers to assign.
 */
export interface MatchedDomain {
  name: string;
  reviewers: string[];
}

/**
 * Result of matching PR files against routing config.
 */
export interface RoutingMatchResult {
  /** Deduplicated list of all reviewers to assign. */
  reviewers: string[];
  /** Domains that matched, with their individual reviewer lists. */
  matchedDomains: MatchedDomain[];
}

/**
 * Check if a file path matches any of the domain's glob patterns.
 *
 * Uses Bun.Glob for pattern matching. Each pattern is tested
 * against the file path independently.
 */
function fileMatchesDomain(filePath: string, domain: Domain): boolean {
  for (const pattern of domain.paths) {
    const glob = new Bun.Glob(pattern);
    if (glob.match(filePath)) {
      return true;
    }
  }
  return false;
}

/**
 * Match a list of PR changed files against the routing config.
 *
 * For each domain, if ANY changed file matches ANY of the domain's
 * path patterns, the domain's reviewers are included in the result.
 *
 * All matched reviewers are collected (union of all matching domains).
 * Reviewers are deduplicated in the final list.
 *
 * @param config - The validated routing configuration
 * @param files - List of changed file paths from the PR
 * @returns Matched reviewers and domain details
 */
export function matchRouting(config: RoutingConfig, files: string[]): RoutingMatchResult {
  const matchedDomains: MatchedDomain[] = [];
  const reviewerSet = new Set<string>();

  for (const domain of config.domains) {
    const domainMatched = files.some((file) => fileMatchesDomain(file, domain));
    if (domainMatched) {
      matchedDomains.push({ name: domain.name, reviewers: domain.reviewers });
      for (const reviewer of domain.reviewers) {
        reviewerSet.add(reviewer);
      }
    }
  }

  return {
    reviewers: Array.from(reviewerSet),
    matchedDomains,
  };
}

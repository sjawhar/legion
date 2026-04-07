import { z } from "zod";

/**
 * A single domain routing rule.
 *
 * Maps a set of file path glob patterns to a list of reviewers
 * who should be assigned when PR files match those patterns.
 */
export const DomainSchema = z.object({
  name: z.string().min(1),
  paths: z.array(z.string().min(1)).min(1),
  reviewers: z.array(z.string().min(1)).min(1),
});

export type Domain = z.infer<typeof DomainSchema>;

/**
 * Top-level routing configuration.
 *
 * Loaded from `.legion/routing.yml` in the workspace root.
 * Defines domain-to-path mappings for reviewer auto-assignment.
 */
export const RoutingConfigSchema = z.object({
  domains: z.array(DomainSchema).min(1),
});

export type RoutingConfig = z.infer<typeof RoutingConfigSchema>;

/** File path for the routing config relative to workspace root. */
export const ROUTING_CONFIG_PATH = ".legion/routing.yml";

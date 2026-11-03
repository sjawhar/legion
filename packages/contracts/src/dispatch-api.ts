/**
 * The Dispatch HTTP and event-bus contract, one module per area. This file is the entry every
 * caller imports (`@legion/contracts` re-exports it whole); nothing outside the package names a
 * module of the split by path.
 *
 * Each event payload is written once: a `z.strictObject` schema and `export type X =
 * z.output<typeof XSchema>` beside it, in the area the payload belongs to. The reading a consumer
 * of the bus applies to that payload sits beside its schema under `Inbound…`, and is that schema
 * with four changes and no more: it keeps only the keys that reading keeps, makes optional the
 * fields a retained frame may omit, keeps unknown fields only where today's reading does, and
 * leaves open the strings a frame may carry a value of that this release does not know
 * (`kind`, `urgency`, an actor's `kind`, a writer's `id`).
 */
export * from "./dispatch-api-asks";
export * from "./dispatch-api-comments";
export * from "./dispatch-api-core";
export * from "./dispatch-api-delivery";
export * from "./dispatch-api-documents";
export * from "./dispatch-api-events";
export * from "./dispatch-api-graph";
export * from "./dispatch-api-inbox";
export * from "./dispatch-api-issues";
export * from "./dispatch-api-limits";
export * from "./dispatch-api-live";
export * from "./dispatch-api-messages";
export * from "./dispatch-api-refs";
export * from "./dispatch-api-requests";
export * from "./dispatch-api-search";
export * from "./dispatch-api-sessions";
export * from "./dispatch-api-targeted";
export * from "./dispatch-api-user";

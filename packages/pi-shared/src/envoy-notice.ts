/** The `details` of an `envoy-message` the Envoy extension writes into its own session (the
 * session-id-changed notice): the session's own doing, never an event from outside, so Legion's
 * phase-stall check does not re-arm a quiet stall on it.
 *
 * It lives in this leaf module, and `./interface` re-exports it for the two plugins that read it
 * there, because `./interface` loads Oh My Pi's logger: the phase-stall machine is bundled by a
 * host that has no Oh My Pi at all (`packages/claude-envoy`, whose bundle runs with no
 * node_modules), so nothing it imports may reach a host module. */
export const LOCAL_ENVOY_NOTICE = { localNotice: true } as const;

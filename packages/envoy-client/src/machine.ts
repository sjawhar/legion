import { hostname } from "node:os";

/**
 * Machine identity reported to Envoy (whoami output, session filters, the registration's `host`).
 * The Go listener stamps registry rows with its configured ENVOY_MACHINE_ID (a logical machine
 * name, set to $(hostname) by the deploy convention), so honor the same variable when present.
 * Else a session inside an agent box reports the machine the box runs on, which the box's launcher
 * names as AGENTBOX_HOST, never the container's own hostname (`agentbox-<hash>`), which names no
 * machine a person or a peer can find. Else the kernel hostname. Every adapter must use this so
 * one host never reports two machine IDs.
 */
export function machineID(): string {
  // biome-ignore lint/complexity/useLiteralKeys: ProcessEnv exposes this key through an index signature.
  return process.env["ENVOY_MACHINE_ID"] || process.env["AGENTBOX_HOST"] || hostname();
}

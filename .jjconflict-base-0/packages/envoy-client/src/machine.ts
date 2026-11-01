import { hostname } from "node:os";

/**
 * Machine identity reported to Envoy (whoami output, session filters).
 * The Go listener stamps registry rows with its configured ENVOY_MACHINE_ID
 * (a logical machine name, set to $(hostname) by the deploy convention), so
 * honor the same variable when present and fall back to the kernel hostname.
 * Every adapter must use this so one host never reports two machine IDs.
 */
export function machineID(): string {
  // biome-ignore lint/complexity/useLiteralKeys: ProcessEnv exposes this key through an index signature.
  return process.env["ENVOY_MACHINE_ID"] || hostname();
}

import { hostname } from "node:os";

/**
 * Machine identity reported to Envoy (registration, whoami, session filters).
 * Every adapter must use this so one host never reports two machine IDs.
 */
export function machineID(): string {
  return hostname();
}

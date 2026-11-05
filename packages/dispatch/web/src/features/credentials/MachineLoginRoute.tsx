import { useQuery } from "@tanstack/react-query";
import { lazy, type ReactNode } from "react";

import { NotFoundPage } from "../shell/NotFoundPage";
import { credentialCapabilitiesQuery } from "./capabilities";

const MachineLoginPage = lazy(() =>
  import("./MachineLoginPage").then((module) => ({ default: module.MachineLoginPage }))
);

/**
 * `/credentials/machine` is served only while this deployment's secrets broker still serves the
 * device grant, which is the only thing that makes a machine print a code for a person to type
 * (`credentialCapabilitiesQuery`, Dispatch's relay of the broker's RFC 8414 metadata). A
 * deployment whose machines sign in with their own cloud identity prints no code, so the page
 * answers like any other address Dispatch does not have.
 *
 * While the answer is still out the route renders nothing rather than "not found": a person who
 * followed a link to a page that does exist must not read that it does not, and the page's own
 * lazy chunk is a blank moment anyway.
 *
 * The gate is the route `app.tsx` names, and the page behind it is this module's own `lazy()`:
 * the gate is a few lines and the ask for the deployment's capabilities starts with the route,
 * while the page stays a chunk of its own, loaded only where the deployment serves it
 * (`e2e/two-builds-routes.ts` walks the route under this name).
 */
export function MachineLoginRoute(): ReactNode {
  const capabilities = useQuery(credentialCapabilitiesQuery());
  if (capabilities.isPending) {
    return null;
  }
  return capabilities.data?.machine_code_page === true ? <MachineLoginPage /> : <NotFoundPage />;
}

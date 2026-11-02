import type { ReportInput } from "../../api/types";
import { runningIndexAsset } from "../shell/DeploymentResilience";

/** What a report says about where it was sent from: everything but the person's words and
 *  whether they ask an agent. */
export type ReportPageContext = Omit<ReportInput, "ask_agent" | "body">;

/** The page a report is sent from, as the browser reports it: its address, the build it runs, the
 *  browser and the window's size in CSS pixels. Read when the report is sent, so the address is
 *  the page the person was on, not the one the dialog opened over. */
export function reportContext(
  view: Pick<Window, "innerHeight" | "innerWidth" | "location" | "navigator"> = window,
  build: () => string = runningIndexAsset
): ReportPageContext {
  return {
    page: view.location.href,
    page_build: build(),
    user_agent: view.navigator.userAgent,
    viewport: { height: Math.round(view.innerHeight), width: Math.round(view.innerWidth) },
  };
}

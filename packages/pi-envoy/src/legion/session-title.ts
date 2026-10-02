import type { PiApi, SessionContext } from "../pi-types";
import type { LegionSessionKind } from "./classify";

/**
 * The title a Legion session gives itself, so Dispatch and Envoy name its author by role and issue
 * rather than by session id (LEGION-205 decision D1): `Legion <role> · <ISSUE>` for an architect or
 * a phase worker, from the `LEGION_ROLE` and `LEGION_ISSUE` both daemons set on every pane and pod,
 * and `Legion controller · <PROJECT>` for a controller, from `LEGION_PROJECT` (the project token).
 * A session Legion does not run gets none.
 */
export function legionSessionTitle(
  session: LegionSessionKind,
  project: string | undefined
): string | undefined {
  switch (session.kind) {
    case "controller":
      // LEGION_PROJECT is the lowercased project token the daemon uses in subjects and paths.
      // Dispatch and Envoy display the canonical uppercase project label.
      return project ? `Legion controller · ${project.toUpperCase()}` : "Legion controller";
    case "root-architect":
      return `Legion architect · ${session.tree}`;
    case "phase-worker":
      return `Legion ${session.role} · ${session.issue}`;
    case "not-legion":
      return undefined;
  }
}

/**
 * Gives the session `title` unless a person already chose its title. Oh My Pi stores a title in the
 * session's header with its source: `auto` for one its title model generated from the first
 * message (an interactive controller's `Legion Controller Start Procedure`), `user` for a rename
 * (`/rename`, the RPC `set_session_name`, or an extension's `pi.setSessionName`, which the host
 * records as `user` too). So a session with no title, or an `auto` one, gets `title`; a resumed
 * session already carrying `title` is left as it is, and so is any other title whose source is not
 * `auto`, a person's rename among them. Dispatch stamps each write's `origin.session_title` and the
 * Envoy registration reads `title` from the same live name (`getSessionName`).
 */
export async function applySessionTitle(
  pi: Pick<PiApi, "setSessionName">,
  context: SessionContext,
  title: string
): Promise<void> {
  const current = context.sessionManager.getSessionName?.();
  if (current === title) return;
  if (current && context.sessionManager.getHeader?.()?.titleSource !== "auto") return;
  await pi.setSessionName(title);
}

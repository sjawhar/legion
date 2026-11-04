import { lazy } from "react";

/** The chunk's request, which `app.tsx`'s `requestTheOpenPagesChunk` makes as its module
 *  evaluates for an address the Inbox opens at, beside the two that open a document. */
export const importInbox = () => import("./Inbox");

/**
 * The Inbox's list, in a chunk of its own.
 *
 * It is the largest thing the shell used to carry in its entry chunk, and no other page draws it:
 * a reader who opened a document downloaded, compiled and evaluated every one of its sections
 * before anything of theirs ran (dispatch://INFRA-341, R11). Both surfaces that show it - the
 * Inbox page (`app.tsx`) and the drawer the shell peeks it in (`InboxDrawer.tsx`) - draw their own
 * frame around a `Suspense` of this component, so each is on the screen exactly when it is today
 * and the list fills in under it, as it does while the list's own read is out.
 */
export const InboxList = lazy(() => importInbox().then((module) => ({ default: module.Inbox })));

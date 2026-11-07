import { useEffect } from "react";

/**
 * Keeps `document.title` in sync with the given title for as long as the calling component is
 * mounted and `active`. Everything the router renders is the page in front and leaves `active`
 * alone; a component the shell keeps mounted behind another page - a live view lingering after
 * the person left it (`features/agent-view/live-views.ts`) - passes `false` and leaves the tab's
 * name to the page they are looking at.
 */
export function useDocumentTitle(title: string, active = true): void {
  useEffect(() => {
    if (!active) return;
    document.title = title;
  }, [active, title]);
}

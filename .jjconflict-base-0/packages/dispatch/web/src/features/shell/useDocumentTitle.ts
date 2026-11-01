import { useEffect } from "react";

/** Keeps `document.title` in sync with the given title for as long as the calling component is mounted. */
export function useDocumentTitle(title: string): void {
  useEffect(() => {
    document.title = title;
  }, [title]);
}

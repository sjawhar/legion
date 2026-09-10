import { useEffect } from "react";

function isTextEditingTarget(target: EventTarget | null): boolean {
  return (
    target instanceof HTMLElement && (target.tagName === "TEXTAREA" || target.isContentEditable)
  );
}

/** Toggles global search without taking over editor and textarea shortcuts. */
export function useSearchShortcut(toggle: () => void): void {
  useEffect(() => {
    const handleKeyDown = (event: KeyboardEvent) => {
      if (
        event.key === "k" &&
        (event.ctrlKey || event.metaKey) &&
        !isTextEditingTarget(event.target)
      ) {
        event.preventDefault();
        toggle();
      }
    };

    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [toggle]);
}

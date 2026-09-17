import { type RefObject, useEffect, useRef, useState } from "react";
import { useLocation } from "react-router-dom";

import { DIALOG_SCOPE, useKeymap, useKeymapScope } from "./keymap";

/** Below Tailwind's `xl`: the margin is a sheet and the sidebar a drawer. */
export const COMPACT_VIEWPORT_QUERY = "(max-width: 1279px)";
/** Below Tailwind's `md`: a phone, where a margin thread opens full-screen. */
export const PHONE_VIEWPORT_QUERY = "(max-width: 767px)";

const focusableSelector = [
  "a[href]",
  "button:not([disabled])",
  "textarea:not([disabled])",
  "input:not([disabled])",
  "select:not([disabled])",
  '[tabindex]:not([tabindex="-1"])',
].join(", ");

// Depth-counted so a picker opening inside an already-locked sheet does not unlock scroll
// when the picker (not the sheet) closes first; only the outermost acquire/release touches
// the DOM, and it restores the exact inline values a page may already have set.
const scrollLock = {
  depth: 0,
  saved: { overflow: "", paddingRight: "" },
  acquire(): void {
    if (this.depth === 0) {
      this.saved = {
        overflow: document.body.style.overflow,
        paddingRight: document.body.style.paddingRight,
      };
      const scrollbarWidth = window.innerWidth - document.documentElement.clientWidth;
      document.body.style.overflow = "hidden";
      if (scrollbarWidth > 0) {
        document.body.style.paddingRight = `${scrollbarWidth}px`;
      }
    }
    this.depth += 1;
  },
  release(): void {
    this.depth = Math.max(0, this.depth - 1);
    if (this.depth === 0) {
      document.body.style.overflow = this.saved.overflow;
      document.body.style.paddingRight = this.saved.paddingRight;
    }
  },
};

// Every open dialog's container. Escape is a `dialog`-scope keymap binding so it works before
// the deferred initial focus has moved into the dialog (the key then lands on whatever was
// focused before, such as the document editor); only the innermost open dialog closes -
// innermost by DOM containment, since a picker nested in a sheet runs its effect first.
const openDialogs = new Set<HTMLElement | null>();

function innermostOpenDialog(): HTMLElement | null | undefined {
  let innermost: HTMLElement | null | undefined;
  for (const container of openDialogs) {
    if (innermost === undefined || (container !== null && innermost?.contains(container))) {
      innermost = container;
    }
  }
  return innermost;
}

export interface UseDialogOptions {
  /** Element to focus first instead of the container's first focusable descendant. */
  initialFocusRef?: RefObject<HTMLElement | null>;
  onClose: () => void;
  open: boolean;
}

export interface DialogHandle<T extends HTMLElement> {
  containerRef: RefObject<T | null>;
}

/**
 * Focus trap, Escape-to-close, focus restoration, and a body scroll lock for an overlay
 * (the search palette, the phone navigation drawer, the phone margin sheet, and the reference
 * picker all use this). While open it pushes the `dialog` keymap scope, which masks every page
 * and global binding, and registers Escape there for the innermost open dialog, whatever
 * element holds focus; a descendant that owns its Escape (the margin Composer dismissing its
 * picker) stops propagation before the dispatcher sees it. Tab trapping listens on the container.
 */
export function useDialog<T extends HTMLElement>({
  initialFocusRef,
  onClose,
  open,
}: UseDialogOptions): DialogHandle<T> {
  const containerRef = useRef<T>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  useKeymapScope(open ? DIALOG_SCOPE : null);
  useKeymap(
    DIALOG_SCOPE,
    open
      ? [
          {
            id: "dialog.close",
            inEditable: true,
            keys: "Escape",
            label: "Close dialog",
            run: () => onCloseRef.current(),
            when: () => innermostOpenDialog() === containerRef.current,
          },
        ]
      : []
  );

  useEffect(() => {
    if (!open) {
      return;
    }
    const container = containerRef.current;
    const previouslyFocused =
      document.activeElement instanceof HTMLElement ? document.activeElement : null;
    scrollLock.acquire();

    // Deferred a frame so a descendant's own synchronous autofocus (e.g. the composer this
    // sheet can open around) has already landed before we decide whether to move focus
    // ourselves — moving it unconditionally here would race that child and steal it back.
    const initialFocusFrame = window.requestAnimationFrame(() => {
      if (container?.contains(document.activeElement)) {
        return;
      }
      const initial =
        initialFocusRef?.current ??
        (container === null
          ? null
          : (container.querySelectorAll<HTMLElement>(focusableSelector)[0] ?? container));
      initial?.focus();
    });

    openDialogs.add(container);

    const handleTab = (event: KeyboardEvent) => {
      if (event.key !== "Tab" || container === null) {
        return;
      }
      const elements = container.querySelectorAll<HTMLElement>(focusableSelector);
      const first = elements[0];
      const last = elements[elements.length - 1];
      if (first === undefined || last === undefined) {
        event.preventDefault();
        return;
      }
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    container?.addEventListener("keydown", handleTab);

    return () => {
      window.cancelAnimationFrame(initialFocusFrame);
      openDialogs.delete(container);
      container?.removeEventListener("keydown", handleTab);
      scrollLock.release();
      if (previouslyFocused !== null && document.contains(previouslyFocused)) {
        previouslyFocused.focus();
      } else {
        document.body.focus({ preventScroll: true });
      }
    };
  }, [open, initialFocusRef]);

  return { containerRef };
}

/** Tracks a media query so mobile-only dialog behaviour does not activate on desktop. */
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => window.matchMedia(query).matches);

  useEffect(() => {
    const list = window.matchMedia(query);
    const update = () => setMatches(list.matches);
    update();
    list.addEventListener("change", update);
    return () => list.removeEventListener("change", update);
  }, [query]);

  return matches;
}

/** Closes an open overlay when the route changes; the mount render itself never closes it. */
export function useCloseOnNavigation(open: boolean, onClose: () => void): void {
  const lastLocationKey = useRef<string | undefined>(undefined);
  const location = useLocation();
  useEffect(() => {
    const changed =
      lastLocationKey.current !== undefined && lastLocationKey.current !== location.key;
    lastLocationKey.current = location.key;
    if (open && changed) {
      onClose();
    }
  }, [location.key, onClose, open]);
}

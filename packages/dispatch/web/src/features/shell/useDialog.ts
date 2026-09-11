import { type RefObject, useEffect, useRef, useState } from "react";

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

// Every open dialog's container. Escape is handled at the document level so it works before
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
 * picker all use this). Escape is observed on `document` in the capture phase and closes the
 * innermost open dialog, whatever element holds focus; Tab trapping listens on the container.
 */
export function useDialog<T extends HTMLElement>({
  initialFocusRef,
  onClose,
  open,
}: UseDialogOptions): DialogHandle<T> {
  const containerRef = useRef<T>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

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

    // Bubble phase on `document`: a descendant that owns its Escape (the margin Composer
    // dismissing its picker or confirming a discard) stops propagation before this runs,
    // while a key that lands outside the dialog still reaches it.
    openDialogs.add(container);
    const handleEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || innermostOpenDialog() !== container) {
        return;
      }
      onCloseRef.current();
    };
    document.addEventListener("keydown", handleEscape);

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
      document.removeEventListener("keydown", handleEscape);
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

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useRef, useSyncExternalStore } from "react";
import { createPortal } from "react-dom";
import { Link } from "react-router-dom";

import { primarySpec } from "../../api/issue-cache";
import { AttentionBadge } from "../../components/Badge";
import { LabelPill, StatusPill } from "../../components/Pill";
import {
  card,
  linkText,
  priorityBadge,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { type IssueStatus, statusLabel } from "../project/board-model";
import { actorLabel } from "./actor";
import { shortForm } from "./RefLink";
import { REF_PREVIEW_CLOSE_DELAY_MS, REF_PREVIEW_OPEN_DELAY_MS } from "./ref-preview-timing";
import {
  buildDispatchReference,
  buildReferencePath,
  type DispatchReferenceRoute,
  isProjectRoute,
  parseDispatchReference,
  referenceTargetKind,
} from "./routes";
import {
  artifactTextQuery,
  excerpt,
  firstLine,
  prefetchReference,
  useReferenceData,
} from "./Unfurl";

const CARD_ID = "ref-preview";
/** Every hover-card trigger: a Markdown anchor `collectReferenceAnchors` tagged, a Proof link
 * mark (`data-dispatch-href`, the sanitized mark's real target), or a React link that set
 * `data-dispatch-ref` — see `referenceTriggerProps`. */
const TRIGGER_SELECTOR = "[data-dispatch-ref],[data-dispatch-href]";

interface PreviewTarget {
  readonly anchor: HTMLElement;
  readonly route: DispatchReferenceRoute;
}

interface PreviewState {
  readonly phase: "pending" | "open";
  readonly target: PreviewTarget;
}

let state: PreviewState | undefined;
let openTimer = 0;
let closeTimer = 0;
const subscribers = new Set<() => void>();

/** A new target, or none, cancels both timers, so a timer's callback always acts on the target
 * that armed it and no transition has to remember to clear them. */
function setState(next: PreviewState | undefined): void {
  if (next?.target !== state?.target) {
    window.clearTimeout(openTimer);
    window.clearTimeout(closeTimer);
  }
  state = next;
  for (const notify of subscribers) {
    notify();
  }
}

/** The one hover card's controller. Module-level on purpose: "one card at a time" is a single
 * piece of state, and `RefPreviewHost` drives it from document-level listeners. Each transition
 * has a precondition its one caller checks - `hoverStart`, `leave` and `reenter` the hover zone
 * (`attachTriggers` alone judges that), `focus` `:focus-visible`, `blur` that focus is not moving
 * into the card, and the scroll handler's `reposition` the anchor it names - so all of them are
 * private and `closeRefPreview` below is the whole outside surface. */
const refPreview = {
  /** Hover began on an anchor outside the hover zone: open its card after the delay unless the
   * pointer leaves first. */
  hoverStart(anchor: HTMLElement, route: DispatchReferenceRoute): void {
    const target: PreviewTarget = { anchor, route };
    setState({ phase: "pending", target });
    openTimer = window.setTimeout(
      () => setState({ phase: "open", target }),
      REF_PREVIEW_OPEN_DELAY_MS
    );
  },
  /** The pointer left the hover zone: a card that has not opened yet is abandoned, and an open
   * one closes after the bridge delay unless the pointer comes back. */
  leave(): void {
    if (state === undefined) {
      return;
    }
    if (state.phase === "pending") {
      setState(undefined);
      return;
    }
    window.clearTimeout(closeTimer);
    closeTimer = window.setTimeout(() => setState(undefined), REF_PREVIEW_CLOSE_DELAY_MS);
  },
  /** The pointer came back into the hover zone before the bridge delay ran out. */
  reenter(): void {
    window.clearTimeout(closeTimer);
  },
  /** Keyboard focus opens the card at once. */
  focus(anchor: HTMLElement, route: DispatchReferenceRoute): void {
    setState({ phase: "open", target: { anchor, route } });
  },
  blur(anchor: HTMLElement): void {
    if (state?.target.anchor === anchor) {
      setState(undefined);
    }
  },
  /** The anchor moved (the page scrolled under a keyboard-opened card): measure it again. */
  reposition(): void {
    if (state !== undefined) {
      setState({ ...state });
    }
  },
};

/** Close the hover card. The one operation with no precondition, so it is the one this module
 * exports: a surface that takes the pointer over (the board's drag lift) closes the card rather
 * than driving the hover transitions itself. */
export function closeRefPreview(): void {
  setState(undefined);
}

function subscribe(notify: () => void): () => void {
  subscribers.add(notify);
  return () => {
    subscribers.delete(notify);
  };
}

/** The attribute a React-rendered link spreads to become a hover-card trigger; the Markdown and
 * Proof surfaces carry the same attributes on the anchors they render themselves. */
export function referenceTriggerProps(route: DispatchReferenceRoute): {
  "data-dispatch-ref": string;
} {
  return { "data-dispatch-ref": buildDispatchReference(route) };
}

function triggerOf(node: EventTarget | null): HTMLElement | null {
  return node instanceof Element ? node.closest<HTMLElement>(TRIGGER_SELECTOR) : null;
}

function cardContains(node: EventTarget | null): boolean {
  return node instanceof Element && node.closest(`#${CARD_ID}`) !== null;
}

/** The one region the pointer may roam without closing the card: the current target's anchor -
 * the open card's, or the one a pending hover is waiting on - and the card itself. Every pointer
 * transition is judged against this predicate, in the document listeners alone, so no close
 * depends on the order in which two event systems see one `pointerout`. */
function inHoverZone(node: EventTarget | null): boolean {
  return state !== undefined && (triggerOf(node) === state.target.anchor || cardContains(node));
}

function routeOf(trigger: HTMLElement): DispatchReferenceRoute | undefined {
  const reference =
    trigger.getAttribute("data-dispatch-ref") ?? trigger.getAttribute("data-dispatch-href");
  return reference === null ? undefined : parseDispatchReference(reference);
}

/** Delegated hover/focus triggers for every reference trigger in the document, present or
 * future — React-rendered links and the anchors inside Markdown bodies and the Proof editor
 * alike, which come and go outside React's knowledge. Returns the detach function. */
function attachTriggers(): () => void {
  const onPointerOver = (event: PointerEvent) => {
    // Inside the zone there is nothing to open: a pointer arriving from outside it only cancels
    // the close its leaving armed, whatever kind of pointer it is.
    if (inHoverZone(event.target)) {
      if (!inHoverZone(event.relatedTarget)) {
        refPreview.reenter();
      }
      return;
    }
    // No card on touch (a tap follows the link), and none while a button is held (a
    // drag-selection passing over a link is not a hover).
    if (event.pointerType === "touch" || event.buttons !== 0) {
      return;
    }
    const trigger = triggerOf(event.target);
    if (trigger === null || trigger === triggerOf(event.relatedTarget)) {
      return;
    }
    const route = routeOf(trigger);
    if (route !== undefined) {
      refPreview.hoverStart(trigger, route);
    }
  };
  const onPointerOut = (event: PointerEvent) => {
    if (inHoverZone(event.target) && !inHoverZone(event.relatedTarget)) {
      refPreview.leave();
    }
  };
  const onFocusIn = (event: FocusEvent) => {
    const trigger = triggerOf(event.target);
    // `:focus-visible` is the browser's own keyboard-versus-pointer verdict: a mouse click or a
    // tap focuses a link without matching it, so only keyboard navigation opens the card.
    if (trigger === null || !trigger.matches(":focus-visible")) {
      return;
    }
    const route = routeOf(trigger);
    if (route !== undefined) {
      refPreview.focus(trigger, route);
    }
  };
  const onFocusOut = (event: FocusEvent) => {
    const trigger = triggerOf(event.target);
    // Focus moving from the anchor into its own card is not leaving: a press on the card's link
    // while the anchor holds focus (dnd-kit hands focus back to a board card's link after a
    // drop) must reach the link as a click, not unmount it under the pointer.
    if (trigger !== null && !cardContains(event.relatedTarget)) {
      refPreview.blur(trigger);
    }
  };
  document.addEventListener("pointerover", onPointerOver);
  document.addEventListener("pointerout", onPointerOut);
  document.addEventListener("focusin", onFocusIn);
  document.addEventListener("focusout", onFocusOut);
  return () => {
    document.removeEventListener("pointerover", onPointerOver);
    document.removeEventListener("pointerout", onPointerOut);
    document.removeEventListener("focusin", onFocusIn);
    document.removeEventListener("focusout", onFocusOut);
  };
}

function Heading({ children }: { children: ReactNode }): ReactNode {
  return <span className={`block font-semibold ${linkText}`}>{children}</span>;
}

function Body({ children }: { children: ReactNode }): ReactNode {
  return <span className={`mt-1 block ${textSecondaryOnSurface}`}>{children}</span>;
}

function IssuePreview({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { issue } = useReferenceData(route);
  const spec = primarySpec(issue);
  const specText = useQuery({
    ...artifactTextQuery(spec?.id, undefined),
    enabled: spec?.kind === "doc",
  });
  if (issue === undefined) {
    return <Heading>{shortForm(route)}</Heading>;
  }
  const specExcerpt =
    specText.data !== undefined && "markdown" in specText.data
      ? excerpt(specText.data.markdown, 200)
      : undefined;
  return (
    <>
      <Heading>
        {issue.key}
        <span className={`ml-2 font-normal ${textPrimaryOnSurface}`}>{issue.title}</span>
      </Heading>
      <span className="mt-2 flex flex-wrap items-center gap-2">
        <StatusPill>{statusLabel(issue.status as IssueStatus)}</StatusPill>
        {issue.priority === null ? null : (
          // Read-only on purpose: the card is a tooltip whose body is one link, so the editable
          // `PriorityControl` every other surface uses cannot live inside it.
          <span
            className={`inline-flex shrink-0 items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold ${priorityBadge[issue.priority].bg} ${priorityBadge[issue.priority].text}`}
          >
            P{issue.priority}
          </span>
        )}
        {issue.labels.map((label) => (
          <LabelPill key={label}>{label}</LabelPill>
        ))}
        {issue.open_asks.length === 0 ? null : (
          <span className={`inline-flex items-center gap-1 text-xs ${textMutedOnSurface}`}>
            <AttentionBadge count={issue.open_asks.length} />
            open {issue.open_asks.length === 1 ? "ask" : "asks"}
          </span>
        )}
      </span>
      {specExcerpt === undefined ? null : <Body>{specExcerpt}</Body>}
    </>
  );
}

function DocumentPreview({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { artifact, markdown } = useReferenceData(route);
  if (artifact === undefined) {
    return <Heading>{shortForm(route)}</Heading>;
  }
  const owner = isProjectRoute(route) ? route.project : route.key;
  const latest = artifact.versions.at(-1)?.number;
  const text = excerpt(markdown, 200);
  return (
    <>
      <Heading>
        <span className={`font-normal ${textMutedOnSurface}`}>{owner} · </span>
        {artifact.name}
      </Heading>
      {latest === undefined ? null : (
        <span className={`mt-1 block text-xs ${textMutedOnSurface}`}>v{latest}</span>
      )}
      {text === undefined ? null : <Body>{text}</Body>}
    </>
  );
}

function AskPreview({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { ask } = useReferenceData(route);
  if (ask === undefined) {
    return <Heading>{shortForm(route)}</Heading>;
  }
  const options = ask.ask.options.length;
  return (
    <>
      <Heading>{ask.ask.question}</Heading>
      <span className={`mt-1 block text-xs ${textMutedOnSurface}`}>
        {ask.ask.state}
        {options === 0 ? null : ` · ${options} ${options === 1 ? "option" : "options"}`}
      </span>
    </>
  );
}

function CommentPreview({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { comment } = useReferenceData(route);
  if (comment === undefined) {
    return <Heading>{shortForm(route)}</Heading>;
  }
  return (
    <>
      <Heading>{actorLabel(comment.comment.author)}</Heading>
      <Body>{firstLine(comment.comment.body)}</Body>
    </>
  );
}

function MessagePreview({ route }: { route: DispatchReferenceRoute }): ReactNode {
  const { message } = useReferenceData(route);
  if (message === undefined) {
    return <Heading>{shortForm(route)}</Heading>;
  }
  return (
    <>
      <Heading>{actorLabel(message.message.author)}</Heading>
      <Body>{firstLine(message.message.body)}</Body>
    </>
  );
}

function PreviewContent({ route }: { route: DispatchReferenceRoute }): ReactNode {
  switch (referenceTargetKind(route)) {
    case "ask":
      return <AskPreview route={route} />;
    case "comment":
      return <CommentPreview route={route} />;
    case "message":
      return <MessagePreview route={route} />;
    case "document":
      return <DocumentPreview route={route} />;
    case "issue":
      return <IssuePreview route={route} />;
  }
}

const VIEWPORT_MARGIN_PX = 8;
const ANCHOR_GAP_PX = 6;

function PreviewCard({ target }: { target: PreviewTarget }): ReactNode {
  const element = useRef<HTMLDivElement>(null);
  // Re-measured on every render: the card grows when its data arrives, and its size decides
  // whether it fits below the anchor. Fixed positioning against the anchor's viewport rect is
  // the boring choice that works in Chromium and iPhone Safari alike — the native Popover API
  // would still need this same arithmetic (CSS anchor positioning is not yet in every engine
  // the e2e projects cover), and the card has no need for the top layer.
  useLayoutEffect(() => {
    const node = element.current;
    if (node === null) {
      return;
    }
    const rect = target.anchor.getBoundingClientRect();
    const height = node.offsetHeight;
    const width = node.offsetWidth;
    const below = rect.bottom + ANCHOR_GAP_PX;
    const above = rect.top - ANCHOR_GAP_PX - height;
    const top =
      below + height <= window.innerHeight - VIEWPORT_MARGIN_PX || above < VIEWPORT_MARGIN_PX
        ? below
        : above;
    const left = Math.max(
      VIEWPORT_MARGIN_PX,
      Math.min(rect.left, window.innerWidth - width - VIEWPORT_MARGIN_PX)
    );
    node.style.top = `${top}px`;
    node.style.left = `${left}px`;
  });
  return (
    <div
      className={`fixed top-0 left-0 z-[60] w-80 max-w-[calc(100vw-1rem)] rounded-xl border p-3 text-sm shadow-lg ${card}`}
      id={CARD_ID}
      ref={element}
      role="tooltip"
    >
      <Link className="block" tabIndex={-1} to={buildReferencePath(target.route)}>
        <PreviewContent route={target.route} />
      </Link>
    </div>
  );
}

/** Mount once in the shell. Owns the document-level trigger listeners, renders the open card
 * into `document.body`, prefetches the target the moment a hover begins (so the card opens
 * populated), links the card to its anchor with `aria-describedby`, and closes it on Escape, on
 * any click (the click follows a link), when the anchor leaves the DOM, or on scroll — unless
 * the anchor holds keyboard focus (focusing it may itself scroll it into view), in which case
 * the card follows the anchor instead. */
export function RefPreviewHost(): ReactNode {
  const current = useSyncExternalStore(subscribe, () => state);
  const queryClient = useQueryClient();

  useEffect(() => {
    const detachTriggers = attachTriggers();
    // Capture phase, ahead of `useDialog`'s Escape: while a card is open it is the topmost
    // layer, so Escape dismisses it alone and leaves the dialog beneath (the search palette)
    // open. With no open card the key passes through untouched.
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape" && state?.phase === "open") {
        event.stopPropagation();
        closeRefPreview();
      }
    };
    const onClose = () => closeRefPreview();
    // Only an open card reacts to scroll. A pending hover is left alone — the pointer often
    // comes to rest on a link while the page is still settling (smooth scrolling, a
    // scrollIntoView), and the card is measured fresh when it opens anyway.
    const onScroll = () => {
      if (state?.phase !== "open") {
        return;
      }
      if (document.activeElement === state.target.anchor) {
        refPreview.reposition();
      } else {
        closeRefPreview();
      }
    };
    document.addEventListener("keydown", onKeyDown, true);
    // Bubble phase, after React's own delegated handlers (the card is portaled into `body`,
    // where React listens): a click on the card must still reach its link and navigate.
    window.addEventListener("click", onClose);
    document.addEventListener("scroll", onScroll, { capture: true, passive: true });
    return () => {
      detachTriggers();
      document.removeEventListener("keydown", onKeyDown, true);
      window.removeEventListener("click", onClose);
      document.removeEventListener("scroll", onScroll, { capture: true });
      closeRefPreview();
    };
  }, []);

  const target = current?.target;
  useEffect(() => {
    if (target === undefined) {
      return;
    }
    prefetchReference(queryClient, target.route);
    // No pointerout or focusout fires for an anchor that leaves the DOM (a Markdown body
    // re-rendering, a route change), so the card would outlive its anchor; watch for removal
    // for as long as this target is live.
    const observer = new MutationObserver(() => {
      if (!target.anchor.isConnected) {
        closeRefPreview();
      }
    });
    observer.observe(document.body, { childList: true, subtree: true });
    return () => observer.disconnect();
  }, [queryClient, target]);

  const openAnchor = current?.phase === "open" ? current.target.anchor : undefined;
  useEffect(() => {
    if (openAnchor === undefined) {
      return;
    }
    const previous = openAnchor.getAttribute("aria-describedby");
    openAnchor.setAttribute("aria-describedby", CARD_ID);
    return () => {
      if (previous === null) {
        openAnchor.removeAttribute("aria-describedby");
      } else {
        openAnchor.setAttribute("aria-describedby", previous);
      }
    };
  }, [openAnchor]);

  if (current?.phase !== "open") {
    return null;
  }
  return createPortal(<PreviewCard target={current.target} />, document.body);
}

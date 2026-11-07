import { type ReactNode, useEffect, useRef } from "react";

import {
  card,
  inlineWarningText,
  progressTrack,
  surfaceMutedHoverBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../theme/classes";

/** The two figures a session's `context` frame carries (INFRA-812). */
export interface ContextUsage {
  /** The current model's context window, in tokens. */
  readonly contextWindow: number;
  /** Context tokens the conversation holds now, as the host estimates them. */
  readonly tokens: number;
}

/** A share this high is nearly full: the ring turns amber and the panel says what follows. */
const NEARLY_FULL_PERCENT = 80;

/** The ring's geometry: a 16 px box, so a 6 px radius leaves room for the 2.5 px stroke. */
const RADIUS = 6;
const CIRCUMFERENCE = 2 * Math.PI * RADIUS;

/** Both figures are counts a person reads at a glance: 68000 is `68,000`. */
const count = new Intl.NumberFormat("en-US");

/** What the panel says about a window that is filling, and about one that is nearly full. The
 *  panel carries no control: compacting the session from here would drive the agent. */
const FILLING_TEXT =
  "When it is nearly full the session drops its oldest turns, so give it less to read, or give the next piece of work to another agent.";
const NEARLY_FULL_TEXT =
  "This window is nearly full. The session drops its oldest turns from here on, so give it less to read, or give the next piece of work to another agent.";

/**
 * The share of the window in use, as a whole percent: 68,000 of 200,000 is 34. A session that
 * published no figures has no share, and neither has a window of zero or a count that is not a
 * finite number — a ring drawn at 0 percent would report a figure the session never gave, which
 * is the one thing R3 forbids. A host that counts more tokens than the window holds reads 100.
 */
export function contextShare(usage: ContextUsage | undefined): number | undefined {
  if (usage === undefined) return undefined;
  const { contextWindow, tokens } = usage;
  if (!Number.isFinite(contextWindow) || contextWindow <= 0) return undefined;
  if (!Number.isFinite(tokens) || tokens < 0) return undefined;
  return Math.min(100, Math.round((tokens / contextWindow) * 100));
}

export interface ContextRingProps {
  /** Closing and opening the panel, so the host can keep one panel of its row open at a time. */
  readonly onOpenChange: (open: boolean) => void;
  readonly open: boolean;
  /** The session's newest `context` frame, or undefined for a session that published none. */
  readonly usage: ContextUsage | undefined;
}

/**
 * How much of its context window a session fills, as a ring with the share beside it, and a
 * panel holding the tokens, the window and what a nearly full window means (INFRA-812, R3). It
 * holds no width of its own beyond its own content, so a row that scrolls sideways at a phone
 * width can keep it at the right edge of the message box.
 *
 * The host owns `open`, as `MultiSelect`'s does. Escape closes the panel wherever the focus is,
 * and so does a pointer press outside it.
 */
export function ContextRing({ onOpenChange, open, usage }: ContextRingProps): ReactNode {
  const share = contextShare(usage);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      // The panel is what Escape acts on while it stands open, not the view behind it.
      event.stopPropagation();
      onOpenChange(false);
      triggerRef.current?.focus();
    };
    const closeOnOutsidePointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (triggerRef.current?.contains(target) || panelRef.current?.contains(target)) return;
      onOpenChange(false);
    };
    document.addEventListener("keydown", closeOnEscape);
    document.addEventListener("pointerdown", closeOnOutsidePointerDown);
    return () => {
      document.removeEventListener("keydown", closeOnEscape);
      document.removeEventListener("pointerdown", closeOnOutsidePointerDown);
    };
  }, [onOpenChange, open]);

  if (usage === undefined || share === undefined) return null;

  const near = share >= NEARLY_FULL_PERCENT;
  const shareText = near ? inlineWarningText : textSecondaryOnCanvas;
  const figures = `${count.format(usage.tokens)} of ${count.format(usage.contextWindow)} tokens`;
  const filled = (CIRCUMFERENCE * share) / 100;

  return (
    <span className="relative shrink-0">
      <button
        aria-expanded={open}
        aria-haspopup="dialog"
        aria-label={`Context window, ${share} percent used`}
        className={`flex min-h-8 shrink-0 items-center gap-1.5 rounded-lg px-1.5 text-xs font-medium tabular-nums ${shareText} ${surfaceMutedHoverBg}`}
        data-testid="agent-context-ring"
        onClick={() => onOpenChange(!open)}
        ref={triggerRef}
        title={figures}
        type="button"
      >
        <svg aria-hidden="true" className="h-4 w-4 shrink-0" fill="none" viewBox="0 0 16 16">
          <circle
            cx="8"
            cy="8"
            r={RADIUS}
            stroke="currentColor"
            strokeOpacity="0.25"
            strokeWidth="2.5"
          />
          <circle
            cx="8"
            cy="8"
            r={RADIUS}
            stroke="currentColor"
            strokeDasharray={`${filled.toFixed(2)} ${CIRCUMFERENCE.toFixed(2)}`}
            strokeLinecap="round"
            strokeWidth="2.5"
            transform="rotate(-90 8 8)"
          />
        </svg>
        <span className="whitespace-nowrap" data-testid="agent-context-share">
          {share}%<span className="hidden sm:inline"> of the window</span>
        </span>
      </button>
      {open ? (
        <div
          aria-label="Context window"
          className={`absolute right-0 bottom-full z-10 mb-2 w-80 max-w-[calc(100vw-1rem)] rounded-lg border p-3 shadow-lg ${card}`}
          data-testid="agent-context-popover"
          ref={panelRef}
          role="dialog"
        >
          <p className={`text-sm font-semibold ${textPrimaryOnSurface}`}>Context window</p>
          <p className={`mt-1 text-xs tabular-nums ${textMutedOnSurface}`}>
            {figures} · {share}%
          </p>
          <span
            aria-hidden="true"
            className={`mt-2 block h-1.5 w-full overflow-hidden rounded-full ${progressTrack}`}
          >
            <span
              className={`block h-full rounded-full bg-current ${shareText}`}
              style={{ width: `${share}%` }}
            />
          </span>
          <p className={`mt-2 text-xs ${textSecondaryOnSurface}`}>
            {near ? NEARLY_FULL_TEXT : FILLING_TEXT}
          </p>
        </div>
      ) : null}
    </span>
  );
}

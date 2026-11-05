import { type KeyboardEvent, type ReactNode, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";

import {
  activeTabIndicatorBorder,
  activeTabIndicatorText,
  borderDefault,
  card,
  focusVisibleRing,
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useMediaQuery } from "../shell/useDialog";
import { presetPath } from "./issue-address";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS, PRESET_NAMES, PRESET_ORDER, type Preset } from "./page-words";

/**
 * The five presets, the node menu beside them and the filter (dispatch://INFRA-1196 R3, R16,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t9`).
 *
 * A preset is a query over the one tree and carries an address of its own, so each one is a
 * link in a tablist: the row says what the page is showing and how much of the tree each query
 * holds, and selecting one is navigation, not a panel switch.
 *
 * The row has two shapes and no third. From 1025 px it is the row of five, each tab named by
 * its preset alone with its count beside the name, hidden from the name so a reader hears
 * "Checks" and not "Checks 6". At 1024 px and below - every phone, and the width the tree
 * itself zooms at - it is one control naming the preset the page is on, which opens a menu of
 * the five. Between the two the row wraps rather than scrolling, so nothing is ever cut.
 *
 * The node menu sits beside the row and outside it, so the one control below 1025 px is
 * unambiguous. What it holds is `dispatch://INFRA-1196/artifact/plan-md#b-t18`'s; what it is -
 * a control named for the issue's files and documents, opening a menu - is here.
 */
export function IssuePresets({
  active,
  counts,
  filter,
  issueKey,
  nodeMenu,
  onFilterChange,
  onLeave,
  toolbar,
}: {
  active: Preset;
  /** How many nodes each preset's query holds, by `presets.ts`. */
  counts: Readonly<Record<string, number>>;
  /** What the filter holds, which is `?q=`. */
  filter: string;
  issueKey: string;
  /** Whether the address opens the node menu, which `/artifacts` does. */
  nodeMenu: boolean;
  onFilterChange: (next: string) => void;
  /** Called before this row leaves the address, so the page records where the reader was. */
  onLeave: () => void;
  /** The open document's toolbar, where the preset has one. */
  toolbar?: ReactNode;
}): ReactNode {
  const navigate = useNavigate();
  const row = useMediaQuery(PRESET_ROW_QUERY);
  // The page opens at most one menu at a time, so which one is open is one value.
  const [open, setOpen] = useState<OpenMenu>(nodeMenu ? "nodes" : undefined);
  const wrapper = useRef<HTMLDivElement>(null);
  const tabs = useRef<Partial<Record<Preset, HTMLAnchorElement | null>>>({});

  // An address that opens the node menu opens it on arrival, and leaving that address closes it
  // again, so the menu says what the address says.
  useEffect(() => setOpen(nodeMenu ? "nodes" : undefined), [nodeMenu]);
  useEffect(() => {
    if (open === undefined) return;
    const closeOutside = (event: PointerEvent) => {
      if (!(event.target instanceof Node) || wrapper.current?.contains(event.target) !== true) {
        setOpen(undefined);
      }
    };
    const closeOnEscape = (event: globalThis.KeyboardEvent) => {
      if (event.key === "Escape") setOpen(undefined);
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [open]);

  const walk = (event: KeyboardEvent<HTMLElement>, from: Preset) => {
    const at = PRESET_ORDER.indexOf(from);
    const to =
      event.key === "ArrowRight"
        ? (at + 1) % PRESET_ORDER.length
        : event.key === "ArrowLeft"
          ? (at - 1 + PRESET_ORDER.length) % PRESET_ORDER.length
          : event.key === "Home"
            ? 0
            : event.key === "End"
              ? PRESET_ORDER.length - 1
              : undefined;
    const next = to === undefined ? undefined : PRESET_ORDER[to];
    if (next === undefined) return;
    event.preventDefault();
    onLeave();
    navigate(presetPath(issueKey, next));
    tabs.current[next]?.focus();
  };

  return (
    <div
      className={`mb-3 flex min-w-0 flex-wrap items-center gap-2 border-b pb-1 ${borderDefault}`}
      ref={wrapper}
    >
      <div className="relative flex min-w-0 flex-wrap items-center" data-testid="issue-presets">
        {row ? (
          <div
            aria-label={CONTROL_LABELS.presets}
            className="flex min-w-0 flex-wrap items-center gap-1"
            role="tablist"
          >
            {PRESET_ORDER.map((preset) => (
              <a
                aria-label={PRESET_NAMES[preset]}
                aria-selected={preset === active}
                className={`inline-flex min-h-11 shrink-0 items-center gap-1.5 px-2 py-2 text-sm md:min-h-9 ${
                  preset === active
                    ? `border-b-2 font-semibold ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
                    : textSecondaryOnCanvas
                }`}
                href={presetPath(issueKey, preset)}
                key={preset}
                onClick={(event) => {
                  if (event.metaKey || event.ctrlKey || event.shiftKey) return;
                  event.preventDefault();
                  onLeave();
                  navigate(presetPath(issueKey, preset));
                }}
                onKeyDown={(event) => walk(event, preset)}
                ref={(node) => {
                  tabs.current[preset] = node;
                }}
                role="tab"
                tabIndex={preset === active ? 0 : -1}
                {...pageChrome}
              >
                <span>{PRESET_NAMES[preset]}</span>
                <span aria-hidden="true" data-preset-count="" {...authorText}>
                  {counts[preset] ?? 0}
                </span>
              </a>
            ))}
          </div>
        ) : (
          <>
            <button
              aria-expanded={open === "presets"}
              aria-haspopup="menu"
              aria-label={PRESET_NAMES[active]}
              className={`inline-flex min-h-11 items-center gap-1.5 rounded-lg px-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${focusVisibleRing}`}
              onClick={() => setOpen((shown) => (shown === "presets" ? undefined : "presets"))}
              type="button"
              {...pageChrome}
            >
              <span>{PRESET_NAMES[active]}</span>
              <span aria-hidden="true" data-preset-count="" {...authorText}>
                {counts[active] ?? 0}
              </span>
            </button>
            {open === "presets" ? (
              <div className={MENU_CLASS} role="menu">
                {PRESET_ORDER.map((preset) => (
                  <button
                    aria-label={PRESET_NAMES[preset]}
                    className={MENU_ITEM_CLASS}
                    key={preset}
                    onClick={() => {
                      setOpen(undefined);
                      onLeave();
                      navigate(presetPath(issueKey, preset));
                    }}
                    role="menuitem"
                    type="button"
                    {...pageChrome}
                  >
                    {PRESET_NAMES[preset]}
                  </button>
                ))}
              </div>
            ) : null}
          </>
        )}
      </div>
      <div className="relative shrink-0">
        <button
          aria-expanded={open === "nodes"}
          aria-haspopup="menu"
          className={`inline-flex min-h-11 items-center rounded-lg px-2 text-sm font-medium md:min-h-9 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${focusVisibleRing}`}
          onClick={() => setOpen((shown) => (shown === "nodes" ? undefined : "nodes"))}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.other_nodes}
        </button>
        {open === "nodes" ? <div className={MENU_CLASS} role="menu" /> : null}
      </div>
      <input
        aria-label={CONTROL_LABELS.filter}
        className={`min-h-11 w-32 min-w-0 flex-1 rounded border px-2 py-1 text-sm md:min-h-9 ${inputClasses(false)}`}
        data-testid="issue-filter"
        onChange={(event) => onFilterChange(event.target.value)}
        type="search"
        value={filter}
      />
      {toolbar}
    </div>
  );
}

/** Which of the page's two menus is open; the page opens at most one at a time. */
type OpenMenu = "nodes" | "presets" | undefined;

/** The width the row of five is drawn at. Below it the row is one control, which is also the
 *  width the tree itself is read one node at a time at (R3, R17). */
const PRESET_ROW_QUERY = "(min-width: 1025px)";

const MENU_CLASS = `absolute left-0 top-full z-30 mt-1 flex w-56 flex-col gap-1 rounded-lg border p-2 shadow-lg ${borderDefault} ${card}`;

const MENU_ITEM_CLASS = `min-h-11 rounded-lg px-2 py-2 text-left text-sm font-medium ${textSecondaryOnSurface}`;

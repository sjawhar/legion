import { type ReactNode, useEffect, useRef, useState } from "react";
import { useLocation } from "react-router-dom";

import {
  backdrop50,
  borderDefault,
  card,
  inputClasses,
  kbdHint,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import type { KeyBindingDescription, KeymapScope } from "./keymap";
import { useDialog } from "./useDialog";

const KEY_GLYPHS: Record<string, string> = {
  ArrowDown: "↓",
  ArrowLeft: "←",
  ArrowRight: "→",
  ArrowUp: "↑",
  Escape: "Esc",
};

/** Each step of a chord as shown (`$mod+k` → `⌘+k`), keyed by its position so `g g` renders twice. */
function keyTokens(keys: string): { key: string; token: string }[] {
  const mod = navigator.userAgent.includes("Mac") ? "⌘" : "Ctrl";
  return keys
    .trim()
    .split(/\s+/)
    .map((combo, position) => ({
      key: `${position}:${combo}`,
      token: combo
        .split("+")
        .map((part) => (part === "$mod" ? mod : (KEY_GLYPHS[part] ?? part)))
        .join("+"),
    }));
}

const SCOPE_LABELS: Record<KeymapScope, string> = {
  board: "Board",
  dialog: "Dialog",
  global: "Global",
  inbox: "Inbox",
  project: "Project",
};

/**
 * Section order in `?`: the shell's own scope, then page scopes outermost first, then dialogs.
 * Registration order would do, except React runs child layout effects first, so a page that
 * mounts straight into its Board would list `board` above `project`.
 */
const SCOPE_ORDER: readonly KeymapScope[] = ["global", "project", "board", "inbox", "dialog"];

/**
 * Every registered shortcut, grouped by scope and live-filtered. `snapshot` is the registry as
 * it stood when `?` was pressed — with focus still on the row the user was on, so per-row
 * bindings report the right availability — and `null` while closed. Inactive rows are greyed,
 * reserved ones say why. A route change closes it, like the search palette.
 */
export function ShortcutHelp({
  onClose,
  snapshot,
}: {
  onClose: () => void;
  snapshot: readonly KeyBindingDescription[] | null;
}): ReactNode {
  const open = snapshot !== null;
  const [filter, setFilter] = useState("");
  const filterRef = useRef<HTMLInputElement>(null);
  const lastLocationKey = useRef<string | undefined>(undefined);
  const location = useLocation();
  const dialog = useDialog<HTMLDivElement>({ initialFocusRef: filterRef, onClose, open });

  useEffect(() => {
    const changed =
      lastLocationKey.current !== undefined && lastLocationKey.current !== location.key;
    lastLocationKey.current = location.key;
    if (open && changed) {
      onClose();
    }
  }, [location.key, onClose, open]);

  if (snapshot === null) {
    return null;
  }

  const needle = filter.trim().toLowerCase();
  const shown =
    needle === ""
      ? snapshot
      : snapshot.filter((entry) =>
          `${entry.label} ${entry.keys.join(" ")} ${entry.scope}`.toLowerCase().includes(needle)
        );
  const scopes = SCOPE_ORDER.filter((scope) => shown.some((entry) => entry.scope === scope));

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={onClose} />
      <div className="pointer-events-none fixed inset-0 z-50 flex items-start justify-center xl:p-4 xl:pt-20">
        <div
          aria-label="Keyboard shortcuts"
          aria-modal="true"
          className={`pointer-events-auto flex max-h-full w-full flex-col overflow-hidden border shadow-2xl xl:max-w-2xl xl:rounded-lg ${card} ${borderDefault}`}
          ref={dialog.containerRef}
          role="dialog"
        >
          <div className={`flex items-center gap-3 border-b p-3 ${borderDefault}`}>
            <h2 className={`text-base font-semibold ${textPrimaryOnSurface}`}>
              Keyboard shortcuts
            </h2>
            <input
              aria-label="Filter shortcuts"
              className={`block min-h-11 min-w-0 flex-1 rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
              onChange={(event) => setFilter(event.target.value)}
              placeholder="Filter"
              ref={filterRef}
              type="search"
              value={filter}
            />
            <button
              className={`min-h-11 rounded-lg px-3 text-sm font-medium ${textMutedOnSurface}`}
              onClick={onClose}
              type="button"
            >
              Close
            </button>
          </div>
          <div className="overflow-y-auto p-3">
            {shown.length === 0 ? (
              <p className={`px-2 py-3 text-sm ${textMutedOnSurface}`}>No shortcuts match</p>
            ) : (
              scopes.map((scope) => (
                <section aria-label={SCOPE_LABELS[scope]} className="mb-4 last:mb-0" key={scope}>
                  <h3
                    className={`mb-1 px-2 text-xs font-semibold tracking-wide uppercase ${textMutedOnSurface}`}
                  >
                    {SCOPE_LABELS[scope]}
                  </h3>
                  <ul>
                    {shown
                      .filter((entry) => entry.scope === scope)
                      .map((entry) => (
                        <li
                          className={`flex min-h-11 items-center justify-between gap-4 rounded-lg px-2 py-1.5 text-sm ${
                            entry.enabled
                              ? textPrimaryOnSurface
                              : `opacity-50 ${textMutedOnSurface}`
                          }`}
                          data-enabled={entry.enabled}
                          key={`${entry.scope}-${entry.id}`}
                        >
                          <span>
                            {entry.label}
                            {entry.reserved === undefined ? null : (
                              <span className={`ml-2 text-xs ${textMutedOnSurface}`}>
                                Reserved — {entry.reserved}
                              </span>
                            )}
                          </span>
                          <KeyHints keys={entry.keys} />
                        </li>
                      ))}
                  </ul>
                </section>
              ))
            )}
          </div>
        </div>
      </div>
    </>
  );
}

const kbdClass = `rounded px-1.5 py-0.5 font-mono text-xs font-medium ${kbdHint}`;

/** `["1", …, "9"]` collapses to `1 – 9`; other alternatives are listed with "or". */
function KeyHints({ keys }: { keys: readonly string[] }): ReactNode {
  const first = keys[0];
  const last = keys[keys.length - 1];
  const consecutive =
    keys.length > 2 &&
    first !== undefined &&
    last !== undefined &&
    keys.every(
      (key, index) => key.length === 1 && key.charCodeAt(0) === first.charCodeAt(0) + index
    );
  if (consecutive) {
    return (
      <span className="flex shrink-0 items-center gap-1">
        <kbd className={kbdClass}>{first}</kbd>
        <span className={`text-xs ${textMutedOnSurface}`}>–</span>
        <kbd className={kbdClass}>{last}</kbd>
      </span>
    );
  }
  return (
    <span className="flex shrink-0 items-center gap-2">
      {keys.map((chord, alternative) => (
        <span className="flex items-center gap-1" key={chord}>
          {alternative === 0 ? null : <span className={`text-xs ${textMutedOnSurface}`}>or</span>}
          {keyTokens(chord).map(({ key, token }) => (
            <kbd className={kbdClass} key={key}>
              {token}
            </kbd>
          ))}
        </span>
      ))}
    </span>
  );
}

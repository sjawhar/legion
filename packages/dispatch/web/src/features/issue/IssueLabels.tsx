import { useQuery } from "@tanstack/react-query";
import {
  type KeyboardEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { createPortal } from "react-dom";

import { api } from "../../api/client";
import {
  borderDefault,
  card,
  cardHoverBorder,
  dangerText,
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  selectedCardBg,
  selectedCardBorder,
  surfaceMutedStrongBg,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";

const maxLabelLength = 40;
const maxLabels = 20;
const optionsId = "issue-label-options";

function labelKey(label: string): string {
  return label.toLowerCase();
}

function optionId(label: string): string {
  return `issue-label-option-${encodeURIComponent(label)}`;
}

function uniqueLabels(labels: readonly string[]): string[] {
  const result: string[] = [];
  const seen = new Set<string>();
  for (const rawLabel of labels) {
    const label = rawLabel.trim();
    if (label === "" || seen.has(labelKey(label))) continue;
    seen.add(labelKey(label));
    result.push(label);
  }
  return result;
}

function includesLabel(labels: readonly string[], label: string): boolean {
  return labels.some((current) => labelKey(current) === labelKey(label));
}

function labelsMatch(left: readonly string[], right: readonly string[]): boolean {
  const leftKeys = new Set<string>();
  const rightKeys = new Set<string>();
  for (const label of left) leftKeys.add(labelKey(label));
  for (const label of right) rightKeys.add(labelKey(label));
  if (leftKeys.size !== rightKeys.size) return false;
  for (const label of leftKeys) {
    if (!rightKeys.has(label)) return false;
  }
  return true;
}

function saveLabels(draft: readonly string[], labels: readonly string[]): string[] {
  return [
    ...labels.filter((label) => includesLabel(draft, label)),
    ...draft.filter((label) => !includesLabel(labels, label)),
  ];
}

export interface IssueLabelsProps {
  readonly disabled: boolean;
  readonly labels: readonly string[];
  readonly onSave: (labels: string[]) => Promise<unknown>;
  readonly project: string;
  readonly saveError: boolean;
  readonly saving: boolean;
  readonly variant?: "default" | "rail";
}

export function IssueLabels({
  disabled,
  labels,
  onSave,
  project,
  saveError,
  saving,
  variant = "default",
}: IssueLabelsProps): ReactNode {
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<string[]>([]);
  const [query, setQuery] = useState("");
  const [inputError, setInputError] = useState<string | null>(null);
  const [activeLabel, setActiveLabel] = useState<string | undefined>(undefined);
  const [popoverPosition, setPopoverPosition] = useState({ left: 0, top: 0 });
  const labelsRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const issues = useQuery({
    enabled: open,
    queryKey: ["issues", "project-labels", project],
    queryFn: () => api.listIssues({ project }),
  });
  const allLabels = useMemo(
    () =>
      uniqueLabels([
        ...draft,
        ...labels,
        ...(issues.data ?? []).flatMap((issue) => issue.labels ?? []),
      ]),
    [draft, issues.data, labels]
  );
  const searchText = query.trim();
  const visibleLabels = useMemo(() => {
    const filter = labelKey(searchText);
    return allLabels
      .filter((label) => labelKey(label).includes(filter))
      .sort((left, right) => {
        const selectedOrder =
          Number(includesLabel(draft, right)) - Number(includesLabel(draft, left));
        return selectedOrder === 0 ? left.localeCompare(right) : selectedOrder;
      });
  }, [allLabels, draft, searchText]);
  const createLabel =
    searchText !== "" && !allLabels.some((label) => labelKey(label) === labelKey(searchText))
      ? searchText
      : undefined;
  const options = createLabel === undefined ? visibleLabels : [...visibleLabels, createLabel];
  const activeOption =
    activeLabel === undefined || !includesLabel(options, activeLabel)
      ? options[0]
      : options.find((label) => labelKey(label) === labelKey(activeLabel));
  const displayLabels = open ? draft : labels;

  const beginEditing = () => {
    if (disabled || saving) return;
    setDraft([...labels]);
    setQuery("");
    setInputError(null);
    setActiveLabel(undefined);
    setOpen(true);
  };
  const close = useCallback(() => {
    if (!open) return;
    const nextLabels = saveLabels(draft, labels);
    setOpen(false);
    setQuery("");
    setInputError(null);
    setActiveLabel(undefined);
    triggerRef.current?.focus();
    if (!labelsMatch(nextLabels, labels)) {
      void onSave(nextLabels).catch(() => undefined);
    }
  }, [draft, labels, onSave, open]);
  const addLabel = (value: string) => {
    const label = value.trim();
    if (label.length === 0 || label.length > maxLabelLength) {
      setInputError(`Labels must be 1–${maxLabelLength} characters.`);
      return;
    }
    if (draft.length >= maxLabels) {
      setInputError(`An issue can have at most ${maxLabels} labels.`);
      return;
    }
    if (includesLabel(draft, label)) {
      setInputError(null);
      return;
    }
    setDraft((current) => [...current, label]);
    setInputError(null);
    setQuery("");
    setActiveLabel(label);
  };
  const toggleLabel = (label: string) => {
    setActiveLabel(label);
    if (includesLabel(draft, label)) {
      setDraft((current) =>
        current.filter((currentLabel) => labelKey(currentLabel) !== labelKey(label))
      );
      setInputError(null);
      return;
    }
    addLabel(label);
  };

  useEffect(() => {
    if (!open) return;
    inputRef.current?.focus();
  }, [open]);

  useLayoutEffect(() => {
    if (!open) return;
    const updatePosition = () => {
      const bounds = triggerRef.current?.getBoundingClientRect();
      if (bounds === undefined) return;
      const minLeft = window.scrollX + 8;
      const maxLeft = Math.max(minLeft, window.scrollX + window.innerWidth - 272);
      const left = Math.min(Math.max(bounds.left + window.scrollX, minLeft), maxLeft);
      const top = bounds.bottom + window.scrollY + 4;
      setPopoverPosition((current) =>
        current.left === left && current.top === top ? current : { left, top }
      );
    };
    const labelsElement = labelsRef.current;
    if (labelsElement === null) return;
    const observer = new MutationObserver(updatePosition);
    observer.observe(labelsElement, { childList: true });
    updatePosition();
    window.addEventListener("resize", updatePosition);
    window.addEventListener("scroll", updatePosition, true);
    return () => {
      observer.disconnect();
      window.removeEventListener("resize", updatePosition);
      window.removeEventListener("scroll", updatePosition, true);
    };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const closeOnOutsidePointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
      if (triggerRef.current?.contains(target) || panelRef.current?.contains(target)) return;
      close();
    };
    document.addEventListener("pointerdown", closeOnOutsidePointerDown);
    return () => document.removeEventListener("pointerdown", closeOnOutsidePointerDown);
  }, [close, open]);

  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "ArrowDown" && options.length > 0) {
      event.preventDefault();
      const index = activeOption === undefined ? -1 : options.indexOf(activeOption);
      setActiveLabel(options[(index + 1) % options.length]);
      return;
    }
    if (event.key === "ArrowUp" && options.length > 0) {
      event.preventDefault();
      const index = activeOption === undefined ? 0 : options.indexOf(activeOption);
      setActiveLabel(options[(index - 1 + options.length) % options.length]);
      return;
    }
    if (event.key === "Enter" && activeOption !== undefined) {
      event.preventDefault();
      toggleLabel(activeOption);
      return;
    }
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      close();
      return;
    }
    if (event.key === "Tab") {
      close();
    }
  };

  return (
    <>
      <div
        className={
          variant === "rail"
            ? "relative flex shrink-0 flex-nowrap items-center gap-1"
            : "relative mt-2 flex flex-wrap items-center gap-2"
        }
        data-testid="issue-labels"
        ref={labelsRef}
      >
        {displayLabels.map((label) => (
          <span
            className={`inline-flex min-h-11 shrink-0 items-center rounded-full px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface}`}
            key={label}
          >
            {label}
          </span>
        ))}
        <button
          aria-expanded={open}
          aria-haspopup="listbox"
          aria-label="Edit labels"
          className={`min-h-11 shrink-0 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
          disabled={disabled || saving}
          onClick={open ? close : beginEditing}
          ref={triggerRef}
          type="button"
        >
          {saving ? "Saving labels…" : displayLabels.length === 0 ? "Labels" : "+"}
        </button>
      </div>
      {open
        ? createPortal(
            <div
              className={`absolute z-50 max-h-72 w-64 max-w-[calc(100vw-1rem)] overflow-auto rounded-lg border shadow-lg ${card} ${borderDefault}`}
              ref={panelRef}
              style={popoverPosition}
            >
              <div className={`border-b p-2 ${borderDefault}`}>
                <input
                  aria-activedescendant={
                    activeOption === undefined ? undefined : optionId(activeOption)
                  }
                  aria-autocomplete="list"
                  aria-controls={optionsId}
                  aria-expanded="true"
                  aria-label="Search or create label"
                  className={`block min-h-11 w-full rounded-lg px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
                  disabled={saving}
                  onChange={(event) => {
                    setQuery(event.target.value);
                    setInputError(null);
                    setActiveLabel(undefined);
                  }}
                  onKeyDown={handleKeyDown}
                  placeholder="Search or create label"
                  ref={inputRef}
                  role="combobox"
                  type="search"
                  value={query}
                />
              </div>
              <div
                aria-label="Label options"
                aria-multiselectable="true"
                id={optionsId}
                role="listbox"
              >
                {visibleLabels.map((label) => {
                  const selected = includesLabel(draft, label);
                  const active =
                    activeOption !== undefined && labelKey(activeOption) === labelKey(label);
                  return (
                    <button
                      aria-selected={selected}
                      className={`flex min-h-11 w-full items-center gap-2 border-b px-3 py-2 text-left text-sm last:border-b-0 ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder} ${active ? `${selectedCardBg} ${selectedCardBorder}` : ""}`}
                      disabled={saving}
                      id={optionId(label)}
                      key={label}
                      onClick={() => toggleLabel(label)}
                      onMouseDown={(event) => event.preventDefault()}
                      role="option"
                      type="button"
                    >
                      <span aria-hidden="true" className="w-4 shrink-0 text-center">
                        {selected ? "✓" : ""}
                      </span>
                      <span className="min-w-0 flex-1 truncate">{label}</span>
                    </button>
                  );
                })}
                {createLabel === undefined ? null : (
                  <button
                    aria-selected="false"
                    className={`flex min-h-11 w-full items-center gap-2 border-b px-3 py-2 text-left text-sm last:border-b-0 ${borderDefault} ${textSecondaryOnSurface} ${cardHoverBorder} ${
                      activeOption !== undefined && labelKey(activeOption) === labelKey(createLabel)
                        ? `${selectedCardBg} ${selectedCardBorder}`
                        : ""
                    }`}
                    disabled={saving}
                    id={optionId(createLabel)}
                    onClick={() => addLabel(createLabel)}
                    onMouseDown={(event) => event.preventDefault()}
                    role="option"
                    type="button"
                  >
                    <span aria-hidden="true" className="w-4 shrink-0 text-center">
                      +
                    </span>
                    Create &quot;{createLabel}&quot;
                  </button>
                )}
                {options.length === 0 ? (
                  <p className={`px-3 py-2 text-sm ${textMutedOnSurface}`}>No labels yet.</p>
                ) : null}
              </div>
              {inputError === null ? null : (
                <p className={`border-t px-3 py-2 text-sm ${borderDefault} ${dangerText}`}>
                  {inputError}
                </p>
              )}
            </div>,
            document.body
          )
        : null}
      {saveError ? <p className={`mt-2 text-sm ${dangerText}`}>Could not save labels.</p> : null}
    </>
  );
}

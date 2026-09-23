import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useCallback, useMemo, useState } from "react";

import { projectIssuesQuery } from "../../api/queries";
import { MultiSelect } from "../../components/MultiSelect";
import {
  dangerText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedStrongBg,
  textSecondaryOnSurface,
} from "../../theme/classes";

const maxLabelLength = 40;
const maxLabels = 20;

function labelKey(label: string): string {
  return label.toLowerCase();
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

/** The issue header's labels: the current chips and the shared `MultiSelect`, which stages a
 *  draft while open and saves the final draft once, when it closes. */
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
  const [inputError, setInputError] = useState<string | null>(null);
  // The same list IssueList and IssueBoard load for this project, so opening the popover on a
  // page that already has it fetches nothing.
  const issues = useQuery({ ...projectIssuesQuery(project, []), enabled: open });
  const options = useMemo(
    () =>
      uniqueLabels([
        ...draft,
        ...labels,
        ...(issues.data ?? []).flatMap((issue) => issue.labels ?? []),
      ]).sort((left, right) => {
        const selectedOrder =
          Number(includesLabel(draft, right)) - Number(includesLabel(draft, left));
        return selectedOrder === 0 ? left.localeCompare(right) : selectedOrder;
      }),
    [draft, issues.data, labels]
  );
  const displayLabels = open ? draft : labels;

  const setOpenState = useCallback(
    (next: boolean) => {
      if (next) {
        if (disabled || saving) return;
        setDraft([...labels]);
        setInputError(null);
        setOpen(true);
        return;
      }
      if (!open) return;
      const nextLabels = saveLabels(draft, labels);
      setOpen(false);
      setInputError(null);
      if (!labelsMatch(nextLabels, labels)) {
        void onSave(nextLabels).catch(() => undefined);
      }
    },
    [disabled, draft, labels, onSave, open, saving]
  );
  const changeDraft = (next: string[]) => {
    if (next.length > maxLabels) {
      setInputError(`An issue can have at most ${maxLabels} labels.`);
      return;
    }
    setDraft(next);
    setInputError(null);
  };
  const createLabel = (value: string) => {
    const label = value.trim();
    if (label.length === 0 || label.length > maxLabelLength) {
      setInputError(`Labels must be 1–${maxLabelLength} characters.`);
      return;
    }
    if (includesLabel(draft, label)) {
      setInputError(null);
      return;
    }
    changeDraft([...draft, label]);
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
      >
        {displayLabels.map((label) => (
          <span
            className={`inline-flex min-h-11 shrink-0 items-center rounded-full px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface}`}
            key={label}
          >
            {label}
          </span>
        ))}
        <MultiSelect
          disabled={disabled || saving}
          emptyMessage="No labels yet."
          error={inputError}
          label="Labels"
          onChange={changeDraft}
          onCreate={createLabel}
          onOpenChange={setOpenState}
          onQueryChange={() => setInputError(null)}
          open={open}
          options={options}
          searchLabel="Search or create label"
          selected={draft}
          triggerAriaLabel="Edit labels"
          triggerClassName={`min-h-11 shrink-0 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
        >
          {saving ? "Saving labels…" : displayLabels.length === 0 ? "Labels" : "+"}
        </MultiSelect>
      </div>
      {saveError ? <p className={`mt-2 text-sm ${dangerText}`}>Could not save labels.</p> : null}
    </>
  );
}

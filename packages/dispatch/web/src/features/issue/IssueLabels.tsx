import { useQuery } from "@tanstack/react-query";
import { type FormEvent, type KeyboardEvent, type ReactNode, useMemo, useState } from "react";

import { api } from "../../api/client";
import {
  borderDefault,
  cardHoverBorder,
  dangerText,
  inputClasses,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedStrongBg,
  textSecondaryOnCanvas,
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

export interface IssueLabelsProps {
  readonly disabled: boolean;
  readonly labels: readonly string[];
  readonly onSave: (labels: string[]) => Promise<unknown>;
  readonly project: string;
  readonly saveError: boolean;
  readonly saving: boolean;
}

export function IssueLabels({
  disabled,
  labels,
  onSave,
  project,
  saveError,
  saving,
}: IssueLabelsProps): ReactNode {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<string[]>([]);
  const [input, setInput] = useState("");
  const [inputError, setInputError] = useState<string | null>(null);
  const issues = useQuery({
    enabled: editing,
    queryKey: ["issues", "project-labels", project],
    queryFn: () => api.listIssues({ project }),
  });
  const suggestions = useMemo(
    () =>
      uniqueLabels((issues.data ?? []).flatMap((issue) => issue.labels ?? []))
        .filter((label) => !draft.some((value) => labelKey(value) === labelKey(label)))
        .sort((left, right) => left.localeCompare(right)),
    [draft, issues.data]
  );

  const beginEditing = () => {
    if (disabled) return;
    setDraft([...labels]);
    setInput("");
    setInputError(null);
    setEditing(true);
  };
  const cancel = () => {
    setInput("");
    setInputError(null);
    setEditing(false);
  };
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
    if (draft.some((current) => labelKey(current) === labelKey(label))) {
      setInputError(null);
      setInput("");
      return;
    }
    setDraft((current) => [...current, label]);
    setInputError(null);
    setInput("");
  };
  const addLabelOnEnter = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Escape") {
      event.preventDefault();
      cancel();
      return;
    }
    if (event.key === "Enter") {
      event.preventDefault();
      addLabel(event.currentTarget.value);
    }
  };
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    void onSave(draft).then(cancel, () => setInputError("Could not save labels."));
  };

  if (!editing) {
    return (
      <div className="mt-2 flex flex-wrap items-center gap-2">
        {labels.map((label) => (
          <button
            className={`min-h-11 rounded-full px-3 py-2 text-xs font-medium ${surfaceMutedStrongBg} ${textSecondaryOnCanvas}`}
            disabled={disabled}
            key={label}
            onClick={beginEditing}
            type="button"
          >
            {label}
          </button>
        ))}
        <button
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
          disabled={disabled}
          onClick={beginEditing}
          type="button"
        >
          {labels.length === 0 ? "Add labels" : "Edit labels"}
        </button>
      </div>
    );
  }

  return (
    <form aria-label="Edit labels" className="mt-2 space-y-2" onSubmit={submit}>
      <div className="flex flex-wrap gap-2">
        {draft.map((label) => (
          <span
            className={`inline-flex min-h-11 items-center gap-1 rounded-full px-3 py-1 text-xs font-medium ${surfaceMutedStrongBg} ${textSecondaryOnCanvas}`}
            key={label}
          >
            {label}
            <button
              aria-label={`Remove ${label}`}
              className={`min-h-11 min-w-11 rounded-full text-sm ${textSecondaryOnCanvas}`}
              disabled={saving}
              onClick={() => setDraft((current) => current.filter((value) => value !== label))}
              type="button"
            >
              ×
            </button>
          </span>
        ))}
      </div>
      <label className={`block text-sm font-medium ${textSecondaryOnCanvas}`}>
        Add label
        <input
          aria-label="Add label"
          className={`mt-1 block min-h-11 w-full rounded-lg px-3 py-2 font-normal outline-none ${inputClasses(true)}`}
          disabled={saving}
          onChange={(event) => {
            setInput(event.target.value);
            setInputError(null);
          }}
          onKeyDown={addLabelOnEnter}
          value={input}
        />
      </label>
      {inputError === null ? null : <p className={`text-sm ${dangerText}`}>{inputError}</p>}
      {suggestions.length === 0 ? null : (
        <fieldset aria-label="Label suggestions" className="flex flex-wrap gap-2">
          {suggestions.map((label) => (
            <button
              className={`min-h-11 rounded-full border px-3 py-2 text-xs font-medium ${borderDefault} ${textSecondaryOnCanvas} ${cardHoverBorder}`}
              disabled={saving}
              key={label}
              onClick={() => addLabel(label)}
              type="button"
            >
              Add {label}
            </button>
          ))}
        </fieldset>
      )}
      <div className="flex flex-wrap gap-2">
        <button
          className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
          disabled={saving}
          type="submit"
        >
          {saving ? "Saving labels…" : "Save labels"}
        </button>
        <button
          className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
          disabled={saving}
          onClick={cancel}
          type="button"
        >
          Cancel
        </button>
      </div>
      {saveError ? <p className={`text-sm ${dangerText}`}>Could not save labels.</p> : null}
    </form>
  );
}

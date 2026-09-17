import { type ReactNode, useMemo, useState } from "react";

import type { ArchitectureTreeComponent } from "../../api/types";
import { MultiSelect } from "../../components/MultiSelect";
import {
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";

export interface ComponentPickerProps {
  /** The trigger's text; defaults to `Components`. */
  readonly children?: ReactNode;
  /** The project's components; external ones are left out here, since the server refuses them. */
  readonly components: readonly ArchitectureTreeComponent[];
  readonly disabled?: boolean;
  /** Shown under the list; a host's failure to load `components`. */
  readonly error?: string | null;
  /** `components` is still on its way: the empty list says so instead of `No components`. */
  readonly loading?: boolean;
  /** The popover opened or closed; a host that fetches `components` only on demand gates on it. */
  readonly onOpenChange?: (open: boolean) => void;
  /** Called once, when the popover closes with a set that differs from `selected`. */
  readonly onSave: (ids: string[]) => void;
  readonly saving: boolean;
  readonly selected: readonly string[];
  readonly triggerAriaLabel: string;
}

/**
 * The one component picker: the shared `MultiSelect` over a project's attachable components,
 * on the Architecture pane's Unassigned rows and on the issue header's `Components:` line. Like
 * the labels editor it stages a draft while open and saves the final draft once, on close.
 */
export function ComponentPicker({
  children,
  components,
  disabled = false,
  error = null,
  loading = false,
  onOpenChange,
  onSave,
  saving,
  selected,
  triggerAriaLabel,
}: ComponentPickerProps): ReactNode {
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<string[]>([]);
  const attachable = useMemo(
    () => components.filter((component) => !component.external),
    [components]
  );
  const titles = useMemo(
    () => Object.fromEntries(attachable.map((component) => [component.id, component.title])),
    [attachable]
  );
  const options = useMemo(
    () =>
      attachable
        .map((component) => component.id)
        .sort((left, right) => {
          const selectedOrder = Number(draft.includes(right)) - Number(draft.includes(left));
          return selectedOrder === 0 ? left.localeCompare(right) : selectedOrder;
        }),
    [attachable, draft]
  );
  const setOpenState = (next: boolean) => {
    if (next) {
      if (disabled || saving) return;
      setDraft([...selected]);
      setOpen(true);
      onOpenChange?.(true);
      return;
    }
    if (!open) return;
    setOpen(false);
    onOpenChange?.(false);
    const sortedDraft = [...draft].sort();
    const sortedSelected = [...selected].sort();
    if (
      sortedDraft.length !== sortedSelected.length ||
      sortedDraft.some((id, index) => id !== sortedSelected[index])
    ) {
      onSave(draft);
    }
  };
  return (
    <MultiSelect
      disabled={disabled || saving}
      emptyMessage={loading ? "Loading components…" : "No components to attach to."}
      error={error}
      label="Components"
      onChange={setDraft}
      onOpenChange={setOpenState}
      open={open}
      optionLabel={(id) => `${id} · ${titles[id] ?? id}`}
      options={options}
      searchLabel="Search components"
      selected={draft}
      triggerAriaLabel={triggerAriaLabel}
      triggerClassName={`min-h-11 shrink-0 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
    >
      {saving ? "Saving…" : (children ?? "Components")}
    </MultiSelect>
  );
}

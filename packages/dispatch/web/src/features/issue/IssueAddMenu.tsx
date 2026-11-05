import { type ReactNode, useEffect, useRef, useState } from "react";

import type { IssueDetails } from "../../api/types";
import {
  borderDefault,
  card,
  focusVisibleRing,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnSurface,
} from "../../theme/classes";
import type { IssueField } from "./IssueFieldForms";
import { PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

/**
 * The one control that reaches a property this issue has no value for (dispatch://INFRA-1196
 * R11). Every property that has a value is drawn where it belongs - in the identity block or in
 * the panel's sections - so nothing is hidden behind this; what it lists is exactly what is not
 * there yet, and picking one opens the control that sets it.
 *
 * Two of the fields it lists are edited by a control the panel draws (the repository and the
 * components), so the menu reaches those by their own accessible names rather than holding a
 * second copy of them, and lists one only while the page draws it: a project that maps no
 * repository offers no repository to set.
 */
export function IssueAddMenu({
  isClosed,
  issue,
  onOpenField,
}: {
  isClosed: boolean;
  issue: IssueDetails;
  onOpenField: (field: IssueField) => void;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const wrapper = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const closeOutside = (event: PointerEvent) => {
      if (!(event.target instanceof Node) || wrapper.current?.contains(event.target) !== true) {
        setOpen(false);
      }
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [open]);

  const empty = emptyFields(issue).filter((field) => field.reachable());
  if (isClosed || empty.length === 0) return null;
  return (
    <div className="relative shrink-0" ref={wrapper}>
      <button
        aria-expanded={open}
        aria-haspopup="menu"
        aria-label={CONTROL_LABELS.add}
        className={`min-h-11 rounded-lg px-2 text-sm font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${focusVisibleRing}`}
        onClick={() => setOpen((shown) => !shown)}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.add}
      </button>
      {open ? (
        <div
          aria-label={CONTROL_LABELS.add}
          className={`absolute left-0 z-30 mt-1 flex w-56 flex-col rounded-lg border p-1 shadow-lg ${borderDefault} ${card}`}
          role="menu"
        >
          {empty.map((field) => (
            <button
              className={`min-h-11 rounded-lg px-2 py-2 text-left text-sm ${textSecondaryOnSurface}`}
              key={field.label}
              onClick={() => {
                setOpen(false);
                field.open();
              }}
              role="menuitem"
              type="button"
              {...pageChrome}
            >
              {field.label}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );

  /** Every property of `issue` with no value, each with the control that sets it. */
  function emptyFields(details: IssueDetails): readonly AddableField[] {
    const byName = (name: string) => () => pageControl(name) !== null;
    const press = (name: string) => () => {
      const control = pageControl(name);
      if (control instanceof HTMLSelectElement) control.focus();
      else control?.click();
    };
    const fields: AddableField[] = [];
    if (details.parent === null) {
      fields.push({
        label: PROPERTY_LABELS.parent,
        open: () => onOpenField("parent"),
        reachable: () => true,
      });
    }
    if (details.assignee === null) {
      fields.push({
        label: PROPERTY_LABELS.assignee,
        open: press(`Assignee of ${details.key}`),
        reachable: byName(`Assignee of ${details.key}`),
      });
    }
    if (details.labels.length === 0) {
      fields.push({
        label: PROPERTY_LABELS.labels,
        open: press("Edit labels"),
        reachable: byName("Edit labels"),
      });
    }
    if (details.route === null || details.route === "") {
      fields.push({
        label: PROPERTY_LABELS.route,
        open: () => onOpenField("route"),
        reachable: () => true,
      });
    }
    if (details.blocked_by.length === 0) {
      fields.push({
        label: PROPERTY_LABELS.blocked_by,
        open: () => onOpenField("blocked_by"),
        reachable: () => true,
      });
    }
    if (details.repo === null) {
      fields.push({
        label: PROPERTY_LABELS.repository,
        open: press(`Repository of ${details.key}`),
        reachable: byName(`Repository of ${details.key}`),
      });
    }
    if (details.components.ids.length === 0) {
      fields.push({
        label: PROPERTY_LABELS.components,
        open: press(`Edit components of ${details.key}`),
        reachable: byName(`Edit components of ${details.key}`),
      });
    }
    return fields;
  }
}

/** One property with no value: what it is called, whether its control is on the page at all, and
 *  how to open it. */
interface AddableField {
  readonly label: string;
  readonly open: () => void;
  readonly reachable: () => boolean;
}

/** The control named `name`, anywhere on the page. The fields the panel draws are edited by
 *  their own controls; this is how the add menu reaches one instead of drawing it twice. */
function pageControl(name: string): HTMLElement | null {
  return document.querySelector<HTMLElement>(`[aria-label="${CSS.escape(name)}"]`);
}

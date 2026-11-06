import { type FocusEvent, type ReactNode, useId, useRef, useState } from "react";

import type { Project } from "../../api/types";
import { Chip } from "../../components/Chip";
import {
  borderDefault,
  card,
  inputClasses,
  surfaceMutedHoverBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { issueStatuses, statusLabel } from "../project/board-model";
import { type SearchBarFilters, type SearchFilterName, searchFilterChips } from "./search-filters";

const field = `min-h-11 rounded-lg border px-3 py-2 text-sm ${inputClasses(true)}`;

/**
 * One value picked from a short list: a trigger that names the filter and a list of plain
 * buttons under it. The list is buttons rather than a `<select>` or a listbox because the
 * palette's own hits are this dialog's `option` rows, and a second set of options inside it
 * would join the rows the arrows walk and the rows a reader is told there are.
 *
 * An item takes the press without taking focus (`onMouseDown`), so the list does not close
 * under the pointer before the click it was opened for.
 */
function FilterMenu({
  emptyText,
  label,
  onPick,
  options,
}: {
  emptyText: string;
  label: string;
  onPick: (value: string) => void;
  options: readonly { text: string; value: string }[];
}): ReactNode {
  const [open, setOpen] = useState(false);
  const listId = useId();
  const wrapper = useRef<HTMLDivElement>(null);
  // Focus leaving the trigger and the list for anything outside them closes the list, as a
  // press outside it would. It rides the buttons rather than the box around them: a box is no
  // control, and a reader who cannot see it is told of no interaction it does not have.
  const closeOnLeave = (event: FocusEvent<HTMLButtonElement>) => {
    if (wrapper.current?.contains(event.relatedTarget) !== true) {
      setOpen(false);
    }
  };

  return (
    <div className="relative" ref={wrapper}>
      <button
        aria-controls={listId}
        aria-expanded={open}
        className={field}
        onBlur={closeOnLeave}
        onClick={() => setOpen(!open)}
        type="button"
      >
        {label}
      </button>
      {open ? (
        <ul
          aria-label={label}
          className={`absolute z-10 mt-1 max-h-64 w-48 overflow-y-auto rounded-lg border py-1 shadow-lg ${card} ${borderDefault}`}
          id={listId}
        >
          {options.length === 0 ? (
            <li className={`px-3 py-2 text-sm ${textMutedOnSurface}`}>{emptyText}</li>
          ) : (
            options.map(({ text, value }) => (
              <li key={value}>
                <button
                  className={`block min-h-11 w-full px-3 py-2 text-left text-sm ${textPrimaryOnSurface} ${surfaceMutedHoverBg}`}
                  onBlur={closeOnLeave}
                  onClick={() => {
                    setOpen(false);
                    onPick(value);
                  }}
                  onMouseDown={(event) => event.preventDefault()}
                  type="button"
                >
                  {text}
                </button>
              </li>
            ))
          )}
        </ul>
      ) : null}
    </div>
  );
}

/**
 * The four narrowings the bar offers, each a control while it is unset and a removable chip
 * once the person has chosen: a project, an issue status, an author and the day since which the
 * item was last written. A chip reads back what was chosen in those words (`Project INFRA`),
 * and pressing it drops that narrowing alone and brings its control back.
 *
 * The author is typed rather than picked: an item's author is a person's login or a session id,
 * and no list holds both. It is taken when the person presses Enter or leaves the box, so a
 * half-typed login never searches.
 */
export function SearchFilterBar({
  filters,
  onFilters,
  projects,
}: {
  filters: SearchBarFilters;
  onFilters: (filters: SearchBarFilters) => void;
  projects: readonly Project[];
}): ReactNode {
  const [author, setAuthor] = useState("");
  const set = (patch: SearchBarFilters) => onFilters({ ...filters, ...patch });
  const takeAuthor = () => {
    const typed = author.trim();
    if (typed !== "") {
      setAuthor("");
      set({ author: typed });
    }
  };
  const removals: Record<SearchFilterName, SearchBarFilters> = {
    author: { author: undefined },
    project: { project: undefined },
    status: { status: undefined },
    updatedSince: { updatedSince: undefined },
  };

  return (
    // A plain row, not a `fieldset`: every group of this dialog is one owner's hits, and a
    // reader told there is one more would look for hits in it.
    <div
      className="flex shrink-0 flex-wrap items-center gap-2 px-3 py-2"
      data-testid="search-filters"
    >
      {searchFilterChips(filters).map(({ label, name }) => (
        <Chip
          aria-label={`Remove ${label}`}
          key={name}
          onClick={() => set(removals[name])}
          removable
          selected
          title={label}
        >
          {label}
        </Chip>
      ))}
      {filters.project === undefined ? (
        <FilterMenu
          emptyText="No projects yet"
          label="Project"
          onPick={(project) => set({ project })}
          options={projects.map((project) => ({ text: project.key, value: project.key }))}
        />
      ) : null}
      {filters.status === undefined ? (
        <FilterMenu
          emptyText="No statuses"
          label="Status"
          onPick={(status) => set({ status })}
          options={issueStatuses.map((status) => ({ text: statusLabel(status), value: status }))}
        />
      ) : null}
      {filters.author === undefined ? (
        <input
          aria-label="Author"
          className={`${field} w-32`}
          onBlur={takeAuthor}
          onChange={(event) => setAuthor(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              event.preventDefault();
              takeAuthor();
            }
          }}
          placeholder="Author"
          type="search"
          value={author}
        />
      ) : null}
      {/* The date box carries its words on screen: a browser draws an empty one as its own date
          format and nothing else, so without them a reader cannot tell what the box narrows. */}
      {filters.updatedSince === undefined ? (
        <label className={`inline-flex items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
          Updated since
          <input
            className={field}
            onChange={(event) => {
              if (event.target.value !== "") {
                set({ updatedSince: event.target.value });
              }
            }}
            type="date"
            value=""
          />
        </label>
      ) : null}
    </div>
  );
}

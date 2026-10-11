import { useMutation, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useRef, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import type { DeliverySessionGroup } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  borderStrong,
  dangerText,
  dismissButtonText,
  inputClasses,
  secondaryButtonCompact,
  surfaceBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { useDialog } from "../shell/useDialog";

const sectionLabel = `text-xs tracking-wide uppercase ${textMutedOnSurface}`;
const fieldClass = `h-8 min-w-0 flex-1 rounded border px-2 text-sm ${inputClasses(true)} ${textPrimaryOnSurface}`;

/** One group: its name, renamable, its members, each removable, and the window's selected
 *  sessions to add. While any group write is pending, every mutating button is disabled so a
 *  second click cannot race the one request that decides the membership. */
function GroupCard({
  group,
  label,
  selected,
  pending,
  write,
}: {
  group: DeliverySessionGroup;
  label: (session: string) => string;
  selected: readonly string[];
  pending: boolean;
  write: (change: () => Promise<unknown>) => void;
}): ReactNode {
  const id = useId();
  const [name, setName] = useState(group.name);
  const addable = selected.filter((session) => !group.sessions.includes(session));
  return (
    <fieldset aria-label={group.name} className={`grid gap-2 rounded border p-3 ${borderDefault}`}>
      <div className="flex items-center gap-2">
        <label className="sr-only" htmlFor={`${id}-name`}>
          Name
        </label>
        <input
          className={fieldClass}
          id={`${id}-name`}
          onChange={(event) => setName(event.target.value)}
          value={name}
        />
        <button
          className={secondaryButtonCompact}
          disabled={pending || name.trim() === "" || name.trim() === group.name}
          onClick={() =>
            write(() => api.updateDeliverySessionGroup(group.id, { name: name.trim() }))
          }
          type="button"
        >
          Rename
        </button>
      </div>
      <ul className="flex flex-wrap gap-1">
        {group.sessions.map((session) => (
          <li
            className={`flex items-center gap-1 rounded border px-2 py-0.5 text-xs ${borderStrong}`}
            key={session}
          >
            <span className="max-w-48 truncate" title={session}>
              {label(session)}
            </span>
            <button
              aria-label={`Remove ${label(session)}`}
              className={dismissButtonText}
              disabled={pending}
              onClick={() =>
                write(() => api.updateDeliverySessionGroup(group.id, { remove: [session] }))
              }
              type="button"
            >
              ✕
            </button>
          </li>
        ))}
      </ul>
      <div className="flex gap-2">
        <button
          className={secondaryButtonCompact}
          disabled={pending || addable.length === 0}
          onClick={() => write(() => api.updateDeliverySessionGroup(group.id, { add: addable }))}
          type="button"
        >
          Add selected
        </button>
        <button
          className={`${secondaryButtonCompact} ${dangerText}`}
          disabled={pending}
          onClick={() => write(() => api.deleteDeliverySessionGroup(group.id))}
          type="button"
        >
          Delete group
        </button>
      </div>
    </fieldset>
  );
}

/** The Delivery page's session-groups editor, opened from the Parent agent facet: the window's
 *  sessions with their titles, pull request counts and groups, to select and name as a new group,
 *  and every group, to rename, add the selected sessions to, remove members from, or delete. A
 *  member no pull request of the window names shows by its id. Each save reads the page again
 *  (every `["delivery"]` query), and a refusal shows as the server wrote it. A dialog
 *  (`useDialog`), at the right edge as the drill-down is, open while it is mounted. */
export function SessionGroupsEditor({
  onClose,
  groups,
  titles,
  counts,
}: {
  onClose: () => void;
  groups: readonly DeliverySessionGroup[];
  titles: Readonly<Record<string, string>>;
  counts: Readonly<Record<string, number>>;
}): ReactNode {
  const id = useId();
  const queryClient = useQueryClient();
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const dialog = useDialog<HTMLElement>({ initialFocusRef: closeButtonRef, onClose, open: true });
  const [selected, setSelected] = useState<string[]>([]);
  const [name, setName] = useState("");
  const save = useMutation({
    mutationFn: (change: () => Promise<unknown>) => change(),
    onSuccess: () => {
      setSelected([]);
      void queryClient.invalidateQueries({ queryKey: ["delivery"] });
    },
  });

  const sessionLabel = (session: string) => {
    const title = titles[session];
    return title === undefined || title === session ? session : `${title} (${session})`;
  };
  const groupOf = new Map<string, string>();
  for (const group of groups) {
    for (const session of group.sessions) groupOf.set(session, group.name);
  }
  const sessions = Object.keys(counts).sort(
    (a, b) => (counts[b] ?? 0) - (counts[a] ?? 0) || sessionLabel(a).localeCompare(sessionLabel(b))
  );
  const toggle = (session: string) =>
    setSelected((current) =>
      current.includes(session) ? current.filter((s) => s !== session) : [...current, session]
    );
  const create = () =>
    save.mutate(() => api.createDeliverySessionGroup({ name: name.trim(), sessions: selected }), {
      onSuccess: () => setName(""),
    });

  return (
    <aside
      aria-labelledby={`${id}-heading`}
      aria-modal="true"
      className={`fixed top-0 right-0 z-30 flex h-full w-[28rem] max-w-full flex-col gap-4 overflow-y-auto border-l p-4 shadow-xl ${borderStrong} ${surfaceBg} ${textPrimaryOnSurface}`}
      ref={dialog.containerRef}
      role="dialog"
    >
      <div className="flex items-start justify-between gap-2">
        <h2 className="text-lg font-semibold" id={`${id}-heading`}>
          Session groups
        </h2>
        <button
          aria-label="Close session groups"
          className={dismissButtonText}
          onClick={onClose}
          ref={closeButtonRef}
          type="button"
        >
          ✕
        </button>
      </div>
      <p className={`text-sm ${textMutedOnSurface}`}>
        A group is one Parent agent: every pull request its sessions wrote first counts under the
        group's name. A session can be in one group.
      </p>

      <section className="grid gap-2">
        <h3 className={sectionLabel} id={`${id}-sessions`}>
          Sessions in this window
        </h3>
        <ul aria-labelledby={`${id}-sessions`} className="grid gap-1">
          {sessions.map((session) => {
            const count = counts[session] ?? 0;
            const inGroup = groupOf.get(session);
            return (
              <li className="flex items-center gap-2 text-sm" key={session}>
                <input
                  aria-label={sessionLabel(session)}
                  checked={selected.includes(session)}
                  id={`${id}-session-${session}`}
                  onChange={() => toggle(session)}
                  type="checkbox"
                />
                <label
                  className="min-w-0 flex-1 truncate"
                  htmlFor={`${id}-session-${session}`}
                  title={session}
                >
                  {sessionLabel(session)}
                </label>
                <span className={`shrink-0 text-xs ${textMutedOnSurface}`}>
                  {count} PR{count === 1 ? "" : "s"}
                  {inGroup === undefined ? "" : ` · ${inGroup}`}
                </span>
              </li>
            );
          })}
        </ul>
        <div className="flex items-center gap-2">
          <label className="sr-only" htmlFor={`${id}-new-name`}>
            New group name
          </label>
          <input
            className={fieldClass}
            id={`${id}-new-name`}
            onChange={(event) => setName(event.target.value)}
            placeholder="Group name"
            value={name}
          />
          <button
            className={secondaryButtonCompact}
            disabled={name.trim() === "" || selected.length === 0 || save.isPending}
            onClick={create}
            type="button"
          >
            Create group
          </button>
        </div>
      </section>

      {save.isError ? (
        <QueryError message={apiErrorMessage(save.error, "Couldn't save the session groups.")} />
      ) : null}

      {groups.length === 0 ? null : (
        <section className="grid gap-2">
          <h3 className={sectionLabel}>Groups</h3>
          {groups.map((group) => (
            <GroupCard
              group={group}
              key={`${group.id}:${group.updated_at}`}
              label={sessionLabel}
              pending={save.isPending}
              selected={selected}
              write={(change) => save.mutate(change)}
            />
          ))}
        </section>
      )}
    </aside>
  );
}

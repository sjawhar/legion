import { type ReactNode, useMemo, useState } from "react";

import type { Agent } from "../../api/types";
import {
  backdrop40,
  card,
  inputClasses,
  surfaceMutedHoverBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { useDialog } from "../shell/useDialog";

export interface Recipient {
  readonly target: string;
  readonly title: string;
  readonly capabilities: readonly string[];
  readonly detail: string;
}

function roleRecipients(agents: readonly Agent[]): Recipient[] {
  const byRole = new Map<string, Agent>();
  for (const agent of agents) {
    for (const role of agent.roles) byRole.set(role, agent);
  }
  return [...byRole.entries()]
    .sort(([left], [right]) => {
      const leftController = left.includes("controller");
      const rightController = right.includes("controller");
      if (leftController !== rightController) return leftController ? -1 : 1;
      return left.localeCompare(right);
    })
    .map(([role, agent]) => ({
      target: `role:${role}`,
      title: role,
      capabilities: agent.capabilities,
      detail: `${agent.title} · ${agent.dir} · ${new Date(agent.last_seen).toLocaleString()}`,
    }));
}

export function recipientOptions(agents: readonly Agent[]): Recipient[] {
  return [
    ...roleRecipients(agents),
    ...[...agents]
      .sort(
        (left, right) => right.last_seen - left.last_seen || left.title.localeCompare(right.title)
      )
      .map((agent) => ({
        target: `session:${agent.session_id}`,
        title: agent.title === "" ? agent.session_id : agent.title,
        capabilities: agent.capabilities,
        detail: `${agent.dir} · ${new Date(agent.last_seen).toLocaleString()}`,
      })),
  ];
}

export function recipientForRoute(
  agents: readonly Agent[],
  route: string | null | undefined
): Recipient | undefined {
  return route === null || route === undefined
    ? undefined
    : recipientOptions(agents).find((recipient) => recipient.target === route);
}

export function RecipientPicker({
  agents,
  onChange,
  onOpenChange,
  value,
}: {
  agents: readonly Agent[];
  onChange: (recipient: Recipient) => void;
  onOpenChange: (open: boolean) => void;
  value: Recipient | null;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const dialog = useDialog<HTMLDivElement>({
    onClose: () => {
      setOpen(false);
      onOpenChange(false);
    },
    open,
  });
  const options = useMemo(() => recipientOptions(agents), [agents]);
  const needle = query.trim().toLowerCase();
  const filtered = options.filter(
    (recipient) =>
      needle === "" ||
      recipient.target.toLowerCase().includes(needle) ||
      recipient.title.toLowerCase().includes(needle) ||
      recipient.detail.toLowerCase().includes(needle)
  );
  const select = (recipient: Recipient) => {
    onChange(recipient);
    setOpen(false);
    onOpenChange(false);
  };

  return (
    <>
      <button
        aria-expanded={open}
        aria-haspopup="dialog"
        aria-label="Choose recipient"
        className={`min-h-11 rounded-lg border px-3 text-left text-sm ${inputClasses(true)}`}
        onClick={() => {
          setOpen(true);
          onOpenChange(true);
        }}
        type="button"
      >
        To: {value?.title ?? "Choose recipient"}
      </button>
      {!open ? null : (
        <>
          <div
            aria-hidden="true"
            className={`fixed inset-0 z-40 ${backdrop40}`}
            onClick={() => {
              setOpen(false);
              onOpenChange(false);
            }}
          />
          <div className="pointer-events-none fixed inset-0 z-40 flex items-end sm:items-start sm:justify-end sm:p-4">
            <section
              aria-label="Recipient picker"
              aria-modal="true"
              className={`pointer-events-auto max-h-[80dvh] w-full overflow-y-auto rounded-t-xl border p-4 shadow-xl sm:max-h-[70vh] sm:max-w-md sm:rounded-xl ${card}`}
              ref={dialog.containerRef}
              role="dialog"
            >
              <label className={`block text-sm font-medium ${textPrimaryOnSurface}`}>
                To
                <input
                  aria-label="Search recipients"
                  className={`mt-2 min-h-11 w-full rounded-lg border px-3 text-sm ${inputClasses(true)}`}
                  onChange={(event) => setQuery(event.target.value)}
                  placeholder="Search agents"
                  type="search"
                  value={query}
                />
              </label>
              <div className="mt-3 flex flex-col gap-1">
                {filtered.map((recipient) => (
                  <button
                    aria-pressed={value?.target === recipient.target}
                    aria-label={`${recipient.title} ${recipient.detail}`}
                    className={`min-h-11 rounded-lg px-3 py-2 text-left ${surfaceMutedHoverBg}`}
                    key={recipient.target}
                    onClick={() => select(recipient)}
                    type="button"
                  >
                    <span className={`block text-sm font-medium ${textPrimaryOnSurface}`}>
                      {recipient.title}
                    </span>
                    <span className={`block truncate text-xs ${textMutedOnSurface}`}>
                      {recipient.detail}
                    </span>
                    <span className={`mt-1 flex gap-1 text-xs ${textMutedOnSurface}`}>
                      {recipient.capabilities.map((capability) => (
                        <span className="rounded border px-1" key={capability}>
                          {capability}
                        </span>
                      ))}
                    </span>
                  </button>
                ))}
              </div>
              {filtered.length === 0 ? (
                <p className={`mt-3 text-sm ${textMutedOnSurface}`}>No live agents match.</p>
              ) : null}
            </section>
          </div>
        </>
      )}
    </>
  );
}

import { type FormEvent, type ReactNode, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import type {
  ArchitectureTree,
  ArchitectureTreeIssueRef,
  ArchitectureTreeNone,
  ArchitectureTreeRetired,
} from "../../api/types";
import { StatusPill } from "../../components/Pill";
import {
  badgeHigh,
  borderDefault,
  card,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { type IssueComponentsWrite, useIssueComponents } from "../issue/useIssueComponents";
import { statusText } from "../project/board-model";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { ComponentPicker } from "./ComponentPicker";

export type AttachmentView = "unassigned" | "none" | "retired";

const secondaryButton = secondaryButtonCompact;

/** The write's server-side refusal, under the row it belongs to. */
function RowError({ error }: { error: string | null }): ReactNode {
  return error === null ? null : (
    <p className={`text-xs ${dangerText}`} role="alert">
      {error}
    </p>
  );
}

function SavingNote(): ReactNode {
  return (
    <span className={`text-xs ${textMutedOnSurface}`} role="status">
      Saving…
    </span>
  );
}

/**
 * The side lists under the tree (spec item 12): **Unassigned** — issues no ancestor chain
 * attached, each with the component picker (`explicit`) and a `Not architectural…` reason form
 * (`none`); **Not architectural** — each with its reason, the ancestor it inherited from, and
 * `Reconsider` (`inherit`) on an issue's own row; **Retired** — issues whose set names components
 * a re-import retired, with the picker to re-attach them. Every write goes through
 * `useIssueComponents`: the row moves in the tree cache at once and stays visible here, in its
 * place, marked saving, until the PATCH resolves; a failure brings it back with the error inline.
 */
export function AttachmentLists({
  project,
  tree,
  view,
}: {
  project: string;
  tree: ArchitectureTree;
  view: AttachmentView;
}): ReactNode {
  if (view === "unassigned") {
    return (
      <ListSection
        emptyMessage="Every issue is attached to a component or declared not architectural."
        heading="Unassigned"
        rows={tree.unassigned}
        testId="unassigned-list"
      >
        {(row, registry) => (
          <UnassignedRow issue={row} project={project} registry={registry} tree={tree} />
        )}
      </ListSection>
    );
  }
  if (view === "none") {
    return (
      <ListSection
        emptyMessage="No issue is declared not architectural."
        heading="Not architectural"
        rows={tree.not_architectural}
        testId="not-architectural-list"
      >
        {(row, registry) => (
          <NotArchitecturalRow issue={row} project={project} registry={registry} />
        )}
      </ListSection>
    );
  }
  return (
    <ListSection
      emptyMessage="No issue points at a retired component."
      heading="Pointing at retired components"
      rows={tree.retired_links}
      testId="retired-list"
    >
      {(row, registry) => (
        <RetiredRow issue={row} project={project} registry={registry} tree={tree} />
      )}
    </ListSection>
  );
}

/** Keeps a row in its list while its write is in flight (`ListSection` owns the set). */
interface InFlightRegistry<Row> {
  finish: (key: string) => void;
  start: (row: Row) => void;
}

interface InFlightRow<Row> {
  /** The row's index in the list when its write started, so it keeps its place. */
  at: number;
  row: Row;
}

function ListSection<Row extends ArchitectureTreeIssueRef>({
  children,
  emptyMessage,
  heading,
  rows,
  testId,
}: {
  children: (row: Row, registry: InFlightRegistry<Row>) => ReactNode;
  emptyMessage: string;
  heading: string;
  rows: readonly Row[];
  testId: string;
}): ReactNode {
  // A row whose write is in flight has already left `rows` (the optimistic tree write moved
  // it); it stays rendered here at its original index, marked saving, until the PATCH resolves.
  const [inFlight, setInFlight] = useState<Record<string, InFlightRow<Row>>>({});
  // The write unmounted the row's controls, so focus fell to the body: once the write settles,
  // it lands on the row's link (a failure keeps the row) or the row now at its index (success).
  const pendingFocus = useRef<number | null>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const registry: InFlightRegistry<Row> = {
    finish: (key) =>
      setInFlight((current) => {
        const { [key]: finished, ...rest } = current;
        if (finished !== undefined) {
          pendingFocus.current = finished.at;
        }
        return rest;
      }),
    start: (row) =>
      setInFlight((current) => ({
        ...current,
        [row.key]: { at: rows.findIndex((candidate) => candidate.key === row.key), row },
      })),
  };
  const visible = [...rows];
  for (const { at, row } of Object.values(inFlight).sort((left, right) => left.at - right.at)) {
    if (!visible.some((candidate) => candidate.key === row.key)) {
      visible.splice(Math.min(at, visible.length), 0, row);
    }
  }
  useEffect(() => {
    const at = pendingFocus.current;
    if (at === null) {
      return;
    }
    pendingFocus.current = null;
    if (document.activeElement !== document.body && document.activeElement !== null) {
      return;
    }
    const items = listRef.current?.children;
    if (items === undefined || items.length === 0) {
      return;
    }
    items[Math.min(at, items.length - 1)]?.querySelector("a")?.focus();
  });
  const headingId = `attachment-list-${testId}`;
  return (
    <section
      aria-labelledby={headingId}
      className={`rounded-xl border p-4 ${card} ${borderDefault}`}
      data-testid={testId}
    >
      <h2 className={`text-lg font-semibold ${textPrimaryOnSurface}`} id={headingId}>
        {heading} <span className={`text-sm font-normal ${textMutedOnSurface}`}>{rows.length}</span>
      </h2>
      {visible.length === 0 ? (
        <p className={`mt-2 text-sm ${textMutedOnSurface}`}>{emptyMessage}</p>
      ) : (
        <ul className="mt-2 space-y-2" ref={listRef}>
          {visible.map((row) => (
            <li
              className={`rounded-lg border px-3 py-2 ${borderDefault}`}
              data-testid={`attachment-${row.key}`}
              key={row.key}
            >
              {children(row, registry)}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** Runs a components write for `issue`, keeping its row in the list while it is in flight. */
function useListWrite<Row extends ArchitectureTreeIssueRef>(
  issue: Row,
  project: string,
  registry: InFlightRegistry<Row>
): IssueComponentsWrite {
  const write = useIssueComponents({ ...issue, project });
  return {
    ...write,
    submit: async (input) => {
      registry.start(issue);
      const accepted = await write.submit(input);
      registry.finish(issue.key);
      return accepted;
    },
  };
}

/** The key · title link and status pill: a full line on phones so the controls stack under it,
 *  sharing the line on wider screens. */
function RowHead({ issue }: { issue: ArchitectureTreeIssueRef }): ReactNode {
  return (
    <div className="flex min-w-0 flex-1 basis-full items-center gap-x-3 sm:basis-0">
      {/* Phone styles.css makes every `a` inline-flex, so `truncate` must sit on an inner
          span: a flex container cannot ellipsize its own nowrap text. */}
      <Link
        className={`flex min-w-0 flex-1 font-medium ${linkText} ${linkHoverText}`}
        to={buildIssuePath({ key: issue.key, kind: "issue" })}
        {...referenceTriggerProps({ key: issue.key, kind: "issue" })}
      >
        <span className="min-w-0 flex-1 truncate">
          {issue.key} · {issue.title}
        </span>
      </Link>
      <StatusPill>{statusText(issue.status)}</StatusPill>
    </div>
  );
}

function UnassignedRow({
  issue,
  project,
  registry,
  tree,
}: {
  issue: ArchitectureTreeIssueRef;
  project: string;
  registry: InFlightRegistry<ArchitectureTreeIssueRef>;
  tree: ArchitectureTree;
}): ReactNode {
  const write = useListWrite(issue, project, registry);
  const [reasonOpen, setReasonOpen] = useState(false);
  const [reason, setReason] = useState("");
  const declareNone = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (reason.trim() === "") {
      return;
    }
    setReasonOpen(false);
    void write.submit({ mode: "none", reason: reason.trim() });
  };
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <RowHead issue={issue} />
        {write.pending ? (
          <SavingNote />
        ) : (
          <>
            <ComponentPicker
              components={tree.components}
              onSave={(ids) => void write.submit({ ids, mode: "explicit" })}
              saving={false}
              selected={[]}
              triggerAriaLabel={`Attach ${issue.key} to components`}
            >
              Attach to…
            </ComponentPicker>
            <button
              aria-expanded={reasonOpen}
              className={secondaryButton}
              onClick={() => setReasonOpen((open) => !open)}
              type="button"
            >
              Not architectural…
            </button>
          </>
        )}
      </div>
      {reasonOpen && !write.pending ? (
        <form className="flex flex-wrap items-end gap-2" onSubmit={declareNone}>
          <label className={`grid flex-1 gap-1 text-xs font-medium ${textSecondaryOnSurface}`}>
            Why {issue.key} is not architectural
            <textarea
              className={`min-h-11 rounded-lg border px-2 py-1 text-sm ${inputClasses(true)} ${textPrimaryOnSurface}`}
              onChange={(event) => setReason(event.target.value)}
              required
              rows={2}
              value={reason}
            />
          </label>
          <button className={secondaryButton} type="submit">
            Save reason
          </button>
        </form>
      ) : null}
      <RowError error={write.error} />
    </div>
  );
}

function NotArchitecturalRow({
  issue,
  project,
  registry,
}: {
  issue: ArchitectureTreeNone;
  project: string;
  registry: InFlightRegistry<ArchitectureTreeNone>;
}): ReactNode {
  const write = useListWrite(issue, project, registry);
  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <RowHead issue={issue} />
        {issue.inherited_from !== null ? (
          <span className={`text-xs ${textMutedOnSurface}`}>
            inherited from{" "}
            <Link
              className={`underline ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ key: issue.inherited_from, kind: "issue" })}
            >
              {issue.inherited_from}
            </Link>
          </span>
        ) : write.pending ? (
          <SavingNote />
        ) : (
          <button
            className={secondaryButton}
            onClick={() => void write.submit({ mode: "inherit" })}
            type="button"
          >
            Reconsider
          </button>
        )}
      </div>
      <p className={`text-sm ${textSecondaryOnSurface}`}>{issue.reason}</p>
      <RowError error={write.error} />
    </div>
  );
}

function RetiredRow({
  issue,
  project,
  registry,
  tree,
}: {
  issue: ArchitectureTreeRetired;
  project: string;
  registry: InFlightRegistry<ArchitectureTreeRetired>;
  tree: ArchitectureTree;
}): ReactNode {
  const write = useListWrite(issue, project, registry);
  // The issue's surviving live attachments: its own set's components (`direct`), or the set it
  // inherits from an ancestor (`inherited`). The picker starts from them so a re-attach adds
  // the pick instead of replacing the whole set with it.
  const live = tree.components
    .filter((component) =>
      component.issues.some(
        (row) =>
          row.key === issue.key && (row.attached === "direct" || row.attached === "inherited")
      )
    )
    .map((component) => component.id);
  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <RowHead issue={issue} />
        {issue.ids.map((id) => (
          <span
            className={`rounded-full px-2 py-0.5 text-xs font-medium ${badgeHigh.bg} ${badgeHigh.text}`}
            key={id}
          >
            retired: {id}
          </span>
        ))}
        {write.pending ? (
          <SavingNote />
        ) : (
          <ComponentPicker
            components={tree.components}
            onSave={(ids) => void write.submit({ ids, mode: "explicit" })}
            saving={false}
            selected={live}
            triggerAriaLabel={`Re-attach ${issue.key} to components`}
          >
            Re-attach to…
          </ComponentPicker>
        )}
      </div>
      <RowError error={write.error} />
    </div>
  );
}

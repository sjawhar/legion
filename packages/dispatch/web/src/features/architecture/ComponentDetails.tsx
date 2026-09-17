import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type {
  ArchitectureTree,
  ArchitectureTreeComponent,
  ArchitectureTreeIssue,
} from "../../api/types";
import { ChevronIcon, DisclosureToggle } from "../../components/DisclosureToggle";
import { StatusPill } from "../../components/Pill";
import {
  badgeLow,
  borderDefault,
  card,
  dangerText,
  disclosureButtonText,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { GitHubLink } from "../issue/GitHubLink";
import { isIssueStatus, statusLabel } from "../project/board-model";
import { MarkdownBody } from "../refs/MarkdownBody";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { expansionStorageKey, placeChild, splitWork } from "./architecture-model";

function readExpansion(storageKey: string): string[] {
  try {
    const raw = window.sessionStorage.getItem(storageKey);
    const parsed: unknown = raw === null ? [] : JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter((key) => typeof key === "string") : [];
  } catch {
    return [];
  }
}

/**
 * A component's details, full width under the rows once `?component=` names it (spec items 7,
 * 9–11): **Work** — its own issue rows, unfinished first by lifecycle status then newest
 * activity, the done ones behind `Show N done`; then **Code & definition** — the prose, `paths`,
 * `depends_on` as links to those components, and the `external` badge. History is not in this
 * slice. A row's chevron expands the issue's children in place; which rows are expanded is kept
 * in `sessionStorage` per project and component, and on mount with `?component=` set the
 * section scrolls into view once, so Back from an issue page lands where the reader left.
 */
export function ComponentDetails({
  component,
  project,
  rowSearch,
  tree,
}: {
  component: ArchitectureTreeComponent;
  project: string;
  rowSearch: (id: string) => string;
  tree: ArchitectureTree;
}): ReactNode {
  const storageKey = expansionStorageKey(project, component.id);
  const [expanded, setExpanded] = useState<string[]>(() => readExpansion(storageKey));
  const [showDone, setShowDone] = useState(false);
  const section = useRef<HTMLElement>(null);
  useEffect(() => {
    section.current?.scrollIntoView({ block: "nearest" });
  }, []);
  const toggleExpanded = (key: string) => {
    setExpanded((current) => {
      const next = current.includes(key)
        ? current.filter((candidate) => candidate !== key)
        : [...current, key];
      window.sessionStorage.setItem(storageKey, JSON.stringify(next));
      return next;
    });
  };
  const { done, open } = splitWork(component.issues);
  const headingId = `component-details-${component.id}`;
  const dependencies = component.depends_on.map((id) => ({
    id,
    title: tree.components.find((candidate) => candidate.id === id)?.title,
  }));
  return (
    <section
      aria-labelledby={headingId}
      className={`rounded-xl border p-4 ${card} ${borderDefault}`}
      data-testid="component-details"
      ref={section}
    >
      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <h2 className={`text-lg font-semibold ${textPrimaryOnSurface}`} id={headingId}>
          {component.title}
        </h2>
        <code className={`text-xs ${textMutedOnSurface}`}>{component.id}</code>
        {component.external ? (
          <span
            className={`rounded-full px-2 py-0.5 text-xs font-medium ${badgeLow.bg} ${badgeLow.text}`}
          >
            external
          </span>
        ) : null}
        {component.external ? null : (
          <span className={`text-sm ${textSecondaryOnSurface}`}>
            {component.done}/{component.total} issues done
            {component.total !== component.own_total
              ? ` · ${component.own_done}/${component.own_total} on this component itself`
              : null}
          </span>
        )}
      </header>

      <h3 className={`mt-4 text-sm font-semibold ${textSecondaryOnSurface}`}>Work</h3>
      {component.issues.length === 0 ? (
        <p className={`mt-1 text-sm ${textMutedOnSurface}`}>
          {component.external
            ? "External components carry no work."
            : "No issues name this component or anything inside it."}
        </p>
      ) : (
        <>
          {open.length === 0 ? (
            <p className={`mt-1 text-sm ${textMutedOnSurface}`}>Every issue here is done.</p>
          ) : (
            <ul aria-label={`Open work on ${component.title}`} className="mt-1 space-y-1">
              {open.map((issue) => (
                <WorkRow
                  component={component.id}
                  expanded={expanded.includes(issue.key)}
                  issue={issue}
                  key={issue.key}
                  onToggle={() => toggleExpanded(issue.key)}
                  rowSearch={rowSearch}
                  tree={tree}
                />
              ))}
            </ul>
          )}
          {done.length === 0 ? null : (
            <div className="mt-2">
              <DisclosureToggle
                expanded={showDone}
                label={`${showDone ? "Hide" : "Show"} ${done.length} done`}
                onToggle={() => setShowDone((current) => !current)}
                textClassName={textSecondaryOnSurface}
              />
              {showDone ? (
                <ul aria-label={`Done work on ${component.title}`} className="space-y-1">
                  {done.map((issue) => (
                    <WorkRow
                      component={component.id}
                      expanded={expanded.includes(issue.key)}
                      issue={issue}
                      key={issue.key}
                      onToggle={() => toggleExpanded(issue.key)}
                      rowSearch={rowSearch}
                      tree={tree}
                    />
                  ))}
                </ul>
              ) : null}
            </div>
          )}
        </>
      )}

      <h3 className={`mt-4 text-sm font-semibold ${textSecondaryOnSurface}`}>
        Code &amp; definition
      </h3>
      {component.prose.trim() === "" ? (
        <p className={`mt-1 text-sm ${textMutedOnSurface}`}>No description in the model.</p>
      ) : (
        <div className={`mt-1 text-sm ${textSecondaryOnSurface}`}>
          <MarkdownBody markdown={component.prose} />
        </div>
      )}
      <dl
        className={`mt-2 grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[auto_1fr] ${textSecondaryOnSurface}`}
      >
        <dt className="font-medium">Paths</dt>
        <dd>
          {component.paths.length === 0 ? (
            <span className={textMutedOnSurface}>None declared</span>
          ) : (
            <ul className="flex flex-wrap gap-x-3 gap-y-1">
              {component.paths.map((path) => (
                <li key={path}>
                  <code className="text-xs">{path}</code>
                </li>
              ))}
            </ul>
          )}
        </dd>
        <dt className="font-medium">Depends on</dt>
        <dd>
          {dependencies.length === 0 ? (
            <span className={textMutedOnSurface}>Nothing</span>
          ) : (
            <ul className="flex flex-wrap gap-x-3 gap-y-1">
              {dependencies.map((dependency) => (
                <li key={dependency.id}>
                  {dependency.title === undefined ? (
                    <code className="text-xs" title="Not in the current model">
                      {dependency.id}
                    </code>
                  ) : (
                    <Link
                      className={`${linkText} ${linkHoverText}`}
                      to={{ search: rowSearch(dependency.id) }}
                    >
                      {dependency.title}
                    </Link>
                  )}
                </li>
              ))}
            </ul>
          )}
        </dd>
      </dl>
    </section>
  );
}

function AttachedMarker({ issue }: { issue: ArchitectureTreeIssue }): ReactNode {
  if (issue.attached === "inherited") {
    return <span className={`text-xs ${textMutedOnSurface}`}>inherited</span>;
  }
  if (issue.attached === "contained" && issue.via !== undefined) {
    return <span className={`text-xs ${textMutedOnSurface}`}>in {issue.via}</span>;
  }
  return null;
}

/** One issue of a component's Work section (spec decision 3: key, title, status, newest
 *  activity, PR links — not assignee, not priority), with a chevron that expands its children. */
function WorkRow({
  component,
  expanded,
  issue,
  onToggle,
  rowSearch,
  tree,
}: {
  component: string;
  expanded: boolean;
  issue: ArchitectureTreeIssue;
  onToggle: () => void;
  rowSearch: (id: string) => string;
  tree: ArchitectureTree;
}): ReactNode {
  return (
    <li className={`rounded-lg border ${borderDefault}`} data-testid={`work-${issue.key}`}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-2 py-1">
        <button
          aria-expanded={expanded}
          aria-label={
            expanded ? `Collapse children of ${issue.key}` : `Expand children of ${issue.key}`
          }
          className={`grid size-11 shrink-0 place-items-center rounded md:size-8 ${disclosureButtonText}`}
          onClick={onToggle}
          type="button"
        >
          <ChevronIcon expanded={expanded} />
        </button>
        {/* Below `sm` the title takes the rest of its own line (basis-full, minus the
            chevron) so the pill and timestamp wrap under it instead of squeezing a
            90 px title into "CORE-1 · We"; from `sm` everything shares one row. */}
        <div className="flex min-w-0 flex-1 basis-[calc(100%-3.5rem)] items-center sm:basis-0">
          <Link
            className={`flex min-w-0 flex-1 font-medium ${linkText} ${linkHoverText}`}
            to={buildIssuePath({ key: issue.key, kind: "issue" })}
            {...referenceTriggerProps({ key: issue.key, kind: "issue" })}
          >
            <span className="min-w-0 flex-1 truncate">
              {issue.key} · {issue.title}
            </span>
          </Link>
        </div>
        <StatusPill>
          {isIssueStatus(issue.status) ? statusLabel(issue.status) : issue.status}
        </StatusPill>
        <Timestamp at={issue.updated_at} className={`text-xs ${textMutedOnSurface}`} />
        {issue.external_links.map((link) => (
          <span className="flex items-center" key={link.url}>
            <GitHubLink link={link} />
          </span>
        ))}
        <AttachedMarker issue={issue} />
      </div>
      {expanded ? (
        <IssueChildren component={component} parent={issue.key} rowSearch={rowSearch} tree={tree} />
      ) : null}
    </li>
  );
}

/** The expanded children of a work row (spec item 9), read from the issue's own detail (the
 *  `["issue", key]` query the issue page shares) and placed against the tree: a child whose row
 *  is this component's renders as a full row; one counted under another component, declared not
 *  architectural, or unassigned renders dimmed with a note saying which. */
function IssueChildren({
  component,
  parent,
  rowSearch,
  tree,
}: {
  component: string;
  parent: string;
  rowSearch: (id: string) => string;
  tree: ArchitectureTree;
}): ReactNode {
  const detail = useQuery({ queryKey: ["issue", parent], queryFn: () => api.getIssue(parent) });
  if (detail.isPending) {
    return <p className={`px-3 pb-2 text-xs ${textMutedOnSurface}`}>Loading children…</p>;
  }
  if (detail.isError) {
    return <p className={`px-3 pb-2 text-xs ${dangerText}`}>Could not load the children.</p>;
  }
  const children = detail.data.children ?? [];
  if (children.length === 0) {
    return <p className={`px-3 pb-2 text-xs ${textMutedOnSurface}`}>No child issues.</p>;
  }
  return (
    <ul aria-label={`Children of ${parent}`} className="space-y-1 px-3 pb-2 pl-12">
      {children.map((child) => {
        const placement = placeChild(tree, component, child.key);
        const dimmed = placement.kind !== "here";
        return (
          <li
            className={`flex flex-wrap items-center gap-x-3 gap-y-1 text-sm ${dimmed ? "opacity-60" : ""}`}
            data-placement={placement.kind}
            key={child.key}
          >
            <Link
              className={`flex min-w-0 max-w-full ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ key: child.key, kind: "issue" })}
              {...referenceTriggerProps({ key: child.key, kind: "issue" })}
            >
              <span className="min-w-0 flex-1 truncate">
                {child.key} · {child.title}
              </span>
            </Link>
            <StatusPill>
              {isIssueStatus(child.status) ? statusLabel(child.status) : child.status}
            </StatusPill>
            {placement.kind === "elsewhere" ? (
              <Link
                className={`text-xs ${textSecondaryOnSurface} ${linkHoverText}`}
                to={{ search: rowSearch(placement.component) }}
              >
                elsewhere · {placement.component}
              </Link>
            ) : placement.kind === "not-architectural" ? (
              <span className={`text-xs ${textMutedOnSurface}`}>not architectural</span>
            ) : placement.kind === "unassigned" ? (
              <span className={`text-xs ${textMutedOnSurface}`}>unassigned</span>
            ) : placement.kind === "uncounted" ? (
              <span className={`text-xs ${textMutedOnSurface}`}>not counted</span>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}

import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { IssueChild, IssueDetails } from "../../api/types";
import {
  borderDefault,
  card,
  linkHoverText,
  linkText,
  progressBarFill,
  surfaceMutedStrongBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName } from "../refs/AgentName";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { GitHubLink } from "./GitHubLink";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS, issueStatusWord } from "./page-words";
import { useChildrenSection } from "./useChildrenSection";

/**
 * The issue's children, one collapsible section at the top of every preset's content
 * (dispatch://INFRA-1196 R10, `dispatch://INFRA-1196/artifact/plan-md#b-t11`).
 *
 * Open, a row says what a reader asks of a child they are waiting on: how it stands, the issue
 * that blocks it, and who holds it. Collapsed, the header keeps the completion bar and both
 * counts, each labelled - the direct children done of total, and the whole subtree done of
 * total - because the two numbers differ and a reader cannot tell which is which from the
 * numbers alone.
 *
 * An issue with no children draws no section at all, so an issue nobody broke down stays quiet.
 * Whether the section is open is the reader's, kept for the next issue they open the way the
 * panel's own sections are kept (`useChildrenSection.ts`).
 */
export function IssueChildren({ issue }: { issue: IssueDetails }): ReactNode {
  const section = useChildrenSection();
  const children = issue.children ?? [];
  if (children.length === 0) {
    return null;
  }
  const direct = {
    done: children.filter((child) => child.status === "done").length,
    total: children.length,
  };
  const subtree = children.reduce(
    (total, child) => ({
      done: total.done + child.subtree_done,
      total: total.total + child.subtree_total,
    }),
    { done: 0, total: 0 }
  );
  return (
    <section
      className={`mb-3 flex min-w-0 flex-col gap-2 border-b pb-2 ${borderDefault}`}
      data-testid="issue-children"
    >
      <div className="flex min-w-0 flex-wrap items-center gap-3">
        <h2 className="min-w-0">
          <button
            aria-expanded={section.isOpen}
            className={`flex min-h-11 items-center gap-2 text-sm font-semibold md:min-h-9 ${textPrimaryOnCanvas}`}
            onClick={section.toggle}
            type="button"
            {...pageChrome}
          >
            <span>{CONTROL_LABELS.children}</span>
            <span aria-hidden="true">{section.isOpen ? "−" : "+"}</span>
          </button>
        </h2>
        <span
          aria-label={CONTROL_LABELS.children_done}
          aria-valuemax={direct.total}
          aria-valuemin={0}
          aria-valuenow={direct.done}
          className={`h-1.5 w-16 shrink-0 overflow-hidden rounded-full ${surfaceMutedStrongBg}`}
          role="progressbar"
        >
          <span
            className={`block h-full rounded-full ${progressBarFill}`}
            style={{ width: `${direct.total === 0 ? 0 : (100 * direct.done) / direct.total}%` }}
          />
        </span>
        <Count done={direct.done} label={CONTROL_LABELS.children_done} total={direct.total} />
        <Count
          done={subtree.done}
          label={CONTROL_LABELS.subtree_done}
          testId="issue-children-subtree-count"
          total={subtree.total}
        />
      </div>
      {section.isOpen ? (
        <ul className="flex min-w-0 flex-col gap-2">
          {children.map((child) => (
            <ChildRow child={child} key={child.key} />
          ))}
        </ul>
      ) : null}
    </section>
  );
}

/** One labelled count: the label is the page's own word and the numbers are the issue's, which
 *  is why they are divided (R21). */
function Count({
  done,
  label,
  testId = "issue-children-direct-count",
  total,
}: {
  done: number;
  label: string;
  testId?: string;
  total: number;
}): ReactNode {
  return (
    <span className={`text-xs ${textMutedOnCanvas}`} data-testid={testId} {...pageChrome}>
      {label} <span {...authorText}>{done}</span>/<span {...authorText}>{total}</span>
    </span>
  );
}

/** One child: what it is, how it stands, what blocks it and who holds it. */
function ChildRow({ child }: { child: IssueChild }): ReactNode {
  return (
    <li
      className={`rounded-lg p-3 ${card}`}
      data-child-row=""
      data-child-status={child.status}
      key={child.key}
    >
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
        <Link
          className={`font-medium ${linkText} ${linkHoverText}`}
          to={buildIssuePath({ key: child.key, kind: "issue" })}
          {...referenceTriggerProps({ key: child.key, kind: "issue" })}
          {...authorText}
        >
          {child.key} · {child.title}
        </Link>
        <span className={`text-sm ${textSecondaryOnSurface}`} {...pageChrome}>
          {issueStatusWord(child.status)}
        </span>
      </div>
      <div
        className={`mt-1 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-sm ${textSecondaryOnSurface}`}
      >
        {child.blocked_by.length === 0 ? null : (
          <span {...pageChrome}>
            {PROPERTY_LABELS.blocked_by}{" "}
            {child.blocked_by.map((key) => (
              <Link
                className={`dispatch-inline-link underline ${linkText} ${linkHoverText}`}
                key={key}
                to={buildIssuePath({ key, kind: "issue" })}
                {...authorText}
              >
                {key}
              </Link>
            ))}
          </span>
        )}
        {child.claim === null ? null : (
          <span {...pageChrome}>
            {PROPERTY_LABELS.claim}{" "}
            <span {...authorText}>
              <AgentName actor={child.claim.actor} inline />
            </span>
          </span>
        )}
        <span {...authorText}>
          <Timestamp at={child.active_at} />
        </span>
        {child.external_links.map((link) => (
          <span className="flex items-center" key={link.url} {...authorText}>
            <GitHubLink link={link} />
          </span>
        ))}
      </div>
    </li>
  );
}

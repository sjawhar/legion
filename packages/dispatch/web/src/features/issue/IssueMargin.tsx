import { type ReactNode, useId, useState } from "react";
import { Link } from "react-router-dom";
import type { IssueDetails } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { NoTooltips } from "../../components/tooltips";
import {
  borderDefault,
  linkHoverText,
  linkText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName } from "../refs/AgentName";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference, buildIssuePath } from "../refs/routes";
import { ConversationStates } from "./conversation-states";
import { FocusedNodeFacts } from "./FocusedNodeFacts";
import type { FocusedNode } from "./focused-node";
import { GitHubLink } from "./GitHubLink";
import { IssueComponentsLine } from "./IssueComponentsLine";
import {
  authorText,
  MARGIN_SECTION_NAMES,
  type MarginSection,
  PROPERTY_LABELS,
  pageChrome,
} from "./page-structure";
import type { ConversationState } from "./page-words";
import { RepoControl } from "./RepoControl";
import { SubscribedAgents } from "./SubscribedAgents";
import { useIssueSubscribers } from "./useIssueSubscribers";
import { useMarginSections } from "./useMarginSections";

/**
 * The issue page's one right-hand panel (dispatch://INFRA-1196 R12): the properties that are not
 * the issue's identity, in three named collapsible sections; the facts of whatever the reader's
 * focus is on; the margin's own content, handed to this panel through the slot
 * (`features/margin/margin-slot.tsx`); and the conversation itself - its four state controls and
 * the one list they drive, which lives here and in no column of the page.
 *
 * It is one panel, not two: the properties are not a second column beside the conversation, and
 * no property of the issue is drawn in both this and the identity block.
 *
 * At the width where the shell's margin becomes a sheet, the page draws this panel in its own
 * column instead, and the sheet keeps the conversation: the properties are read at every width
 * without opening anything (R11).
 */
export function IssueMarginPanel({
  conversation,
  conversationList,
  conversationState,
  focused,
  isClosed,
  issue,
  onConversationState,
}: {
  /** The margin's own content, where this panel sits inside the shell's margin. */
  conversation?: ReactNode;
  /** The conversation's one list, which the state controls above it drive. It reads at the
   *  panel's own width (384 px by default, 280 px at its narrowest). */
  conversationList?: ReactNode;
  /** Which state the conversation is in, which the address names and these controls move. */
  conversationState: ConversationState;
  /** What the reader's focus is on, which the page reads and hands over: this panel is drawn
   *  where the margin is, so it is outside the page's own React tree and reads no context of
   *  it (`focused-node.tsx`). */
  focused: FocusedNode;
  isClosed: boolean;
  issue: IssueDetails;
  /** Takes the state a reader picked, which the page turns into its address. */
  onConversationState: (state: ConversationState) => void;
}): ReactNode {
  const sections = useMarginSections();
  const subscribers = useIssueSubscribers(issue.key);
  const [subscribersOpen, setSubscribersOpen] = useState(false);
  const [referencesOpen, setReferencesOpen] = useState(false);
  const referencesPanelId = useId();
  const issueReference = buildDispatchReference({ key: issue.key, kind: "issue" });

  return (
    // The panel is the page's, wherever the shell mounts it, and the page draws no tooltips
    // (`components/tooltips.tsx`): the margin's own cards reach that reading through this
    // wrapper, because they are rendered inside it (`features/margin/margin-slot.tsx`).
    <NoTooltips>
      <div className="flex min-w-0 flex-col gap-3" data-testid="issue-margin">
        <PropertySection name="development" sections={sections}>
          <RepoControl
            disabled={isClosed}
            issueKey={issue.key}
            project={issue.project}
            repo={issue.repo}
            resolvedRepo={issue.resolved_repo}
          />
          <IssueComponentsLine issue={issue} />
          {issue.blocked_by.length === 0 ? null : (
            <Property label={PROPERTY_LABELS.blocked_by}>
              {issue.blocked_by.map((key) => (
                <Link
                  className={`dispatch-inline-link underline ${linkText} ${linkHoverText}`}
                  key={key}
                  to={buildIssuePath({ key, kind: "issue" })}
                  {...authorText}
                >
                  {key}
                </Link>
              ))}
            </Property>
          )}
          {issue.external_links.length === 0 ? null : (
            <Property label={PROPERTY_LABELS.external_links}>
              {issue.external_links.map((link) => (
                <GitHubLink key={link.url} link={link} />
              ))}
            </Property>
          )}
        </PropertySection>
        <PropertySection name="people" sections={sections}>
          {issue.created_by == null ? null : (
            <Property label={PROPERTY_LABELS.opened_by}>
              <span {...authorText}>
                <AgentName actor={issue.created_by} inline />
              </span>
            </Property>
          )}
          <button
            aria-expanded={subscribersOpen}
            className={`flex min-h-11 items-center gap-2 rounded-lg px-2 py-1 text-sm font-medium ${textSecondaryOnSurface}`}
            onClick={() => setSubscribersOpen((open) => !open)}
            type="button"
            {...pageChrome}
          >
            <span>{PROPERTY_LABELS.subscribers}:</span>
            <span>{subscribers.rows.length}</span>
          </button>
          {subscribersOpen ? (
            <SubscribedAgents
              onUnsubscribe={(sessionId) => subscribers.unsubscribe.mutate(sessionId)}
              ownerLabel={issue.key}
              subscribers={subscribers.rows}
            />
          ) : null}
          {subscribers.error ? (
            <QueryError
              message="Subscribed agents unavailable — Envoy listener unreachable."
              onRetry={subscribers.refetch}
              retrying={subscribers.fetching}
            />
          ) : null}
          {subscribers.unsubscribe.isError ? (
            <QueryError
              message="Could not unsubscribe this agent."
              onRetry={() =>
                subscribers.unsubscribe.mutate(subscribers.unsubscribe.variables ?? "")
              }
              retrying={subscribers.unsubscribe.isPending}
            />
          ) : null}
        </PropertySection>
        <PropertySection name="references" sections={sections}>
          <ReferencedByToggle
            controls={referencesPanelId}
            count={issue.referenced_by_count}
            expanded={referencesOpen}
            onToggle={() => setReferencesOpen((open) => !open)}
          />
          {referencesOpen ? (
            <ReferencedBy
              className="w-full"
              reference={issueReference}
              toggle={{ id: referencesPanelId }}
            />
          ) : null}
        </PropertySection>
        <FocusedNodeFacts node={focused} />
        {conversation}
        {/* Under the margin's own content, not above it: the margin places a landed card at a
          `top` it measures from the content above it, and a section inserted over that content
          moves the anchor it measured against. */}
        <ConversationStates onPick={onConversationState} state={conversationState} />
        {conversationList}
      </div>
    </NoTooltips>
  );
}

/** One named section of the panel's properties, open or closed by its own one control. A closed
 *  section draws none of its values, so what a reader sees is what the page holds. */
function PropertySection({
  children,
  name,
  sections,
}: {
  children: ReactNode;
  name: MarginSection;
  sections: ReturnType<typeof useMarginSections>;
}): ReactNode {
  const open = sections.isOpen(name);
  return (
    <section className={`border-b pb-2 ${borderDefault}`} data-margin-section={name}>
      <h2>
        <button
          aria-expanded={open}
          className={`flex min-h-11 w-full items-center justify-between gap-2 text-sm font-semibold ${textPrimaryOnSurface}`}
          onClick={() => sections.toggle(name)}
          type="button"
          {...pageChrome}
        >
          <span>{MARGIN_SECTION_NAMES[name]}</span>
          <span aria-hidden="true">{open ? "−" : "+"}</span>
        </button>
      </h2>
      {open ? <div className="flex min-w-0 flex-col gap-2">{children}</div> : null}
    </section>
  );
}

/** One property of the issue inside a section: its name, then its value. */
function Property({ children, label }: { children: ReactNode; label: string }): ReactNode {
  return (
    <div className={`flex min-w-0 flex-wrap items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
      <span className={`font-medium ${textMutedOnSurface}`} {...pageChrome}>
        {label}:
      </span>
      {children}
    </div>
  );
}

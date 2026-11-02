import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type {
  Artifact,
  Decision,
  DecisionLink,
  IssueAskRow,
  IssueDecisions,
} from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { QueryError } from "../../components/QueryError";
import {
  card,
  dangerText,
  decisionUnsureAccent,
  inputClasses,
  linkHoverText,
  linkText,
  primaryButton,
  secondaryButtonCompact,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { blockFragment } from "../refs/block-reference";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath, DECISIONS_DOCUMENT_SLUG, documentItemPath } from "../refs/routes";
import { useKeyedWrite } from "../writes/useKeyedWrite";

/** The paragraphs of a decision's markdown below its heading, each marked unsure or not. A
 *  paragraph that starts `Unsure:` is the one the server reads as the decision's doubt. */
function decisionParagraphs(text: string): { markdown: string; unsure: boolean }[] {
  return text
    .split(/\n\s*\n/)
    .map((paragraph) => paragraph.trim())
    .filter((paragraph) => paragraph !== "" && !paragraph.startsWith("#"))
    .map((markdown) => ({ markdown, unsure: markdown.startsWith("Unsure:") }));
}

/** The path that opens the issue's spec at `blockId`: through the spec's own document route when
 *  the issue's artifacts are loaded, as every in-app block link is built. */
function specBlockPath(issueKey: string, spec: Artifact | undefined, blockId: string): string {
  return spec === undefined
    ? `${buildIssuePath({ key: issueKey, kind: "spec" })}${blockFragment(blockId)}`
    : documentItemPath(spec, { blockID: blockId, id: blockId, kind: "comment" });
}

function Section({
  children,
  empty,
  title,
}: {
  children: ReactNode[];
  empty: string;
  title: string;
}): ReactNode {
  const id = useId();
  return (
    <section aria-labelledby={id} className="space-y-3">
      <h2 className={`text-xs font-semibold uppercase ${textSecondaryOnSurface}`} id={id}>
        {title}
      </h2>
      {children.length === 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`}>{empty}</p>
      ) : (
        <ul className="space-y-3">{children}</ul>
      )}
    </section>
  );
}

function ShapedLinks({
  issueKey,
  links,
  spec,
}: {
  issueKey: string;
  links: readonly DecisionLink[];
  spec: Artifact | undefined;
}): ReactNode {
  const shaped = links.filter((link) => link.to.block_id !== undefined);
  if (shaped.length === 0) {
    return null;
  }
  return (
    <div className="space-y-1">
      <p className={`text-xs ${textMutedOnSurface}`}>Shaped in the spec:</p>
      <ul className="space-y-1 text-sm">
        {shaped.map((link) => {
          const blockId = link.to.block_id ?? "";
          return (
            <li key={`${link.from_block}:${blockId}`}>
              <Link
                className={`${linkText} ${linkHoverText}`}
                to={specBlockPath(issueKey, spec, blockId)}
              >
                {link.to.text?.trim() || `Block ${blockId}`}
              </Link>
              {link.stale ? (
                <span className={`ml-2 text-xs ${textMutedOnSurface}`}>out of date</span>
              ) : null}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function ChangeComposer({
  decision,
  documentId,
  onDone,
  quote,
}: {
  decision: Decision;
  documentId: string;
  onDone: () => void;
  quote: string;
}): ReactNode {
  const [body, setBody] = useState("");
  const fieldId = useId();
  const send = useKeyedWrite({
    inline: true,
    label: () => `Change to ${decision.heading}`,
    write: (input: { body: string; quote: string }, bounds) =>
      api.createArtifactComment(
        documentId,
        { anchor: { artifact: documentId, quote: input.quote }, body: input.body },
        bounds
      ),
    onSuccess: () => {
      setBody("");
      onDone();
    },
  });
  return (
    <form
      aria-label={`Change ${decision.heading}`}
      className="space-y-2"
      onSubmit={(event) => {
        event.preventDefault();
        if (body.trim() !== "") {
          send.mutate({ body, quote });
        }
      }}
    >
      <blockquote
        className={`border-l-2 pl-2 text-sm ${textSecondaryOnSurface}`}
        data-testid="decision-quote"
      >
        {quote}
      </blockquote>
      <label className={`block text-sm ${textPrimaryOnSurface}`} htmlFor={fieldId}>
        Your change
      </label>
      <textarea
        className={`block w-full rounded-lg border px-3 py-2 text-sm outline-none ${inputClasses(true)}`}
        id={fieldId}
        onChange={(event) => setBody(event.target.value)}
        rows={3}
        value={body}
      />
      {send.error === null ? null : (
        <p className={`text-sm ${dangerText}`} role="alert">
          {send.error.message}
        </p>
      )}
      <div className="flex gap-2">
        <button
          className={primaryButton}
          disabled={send.isPending || body.trim() === ""}
          type="submit"
        >
          Send
        </button>
        <button className={secondaryButtonCompact} onClick={onDone} type="button">
          Cancel
        </button>
      </div>
    </form>
  );
}

function DecisionCard({
  actions,
  decision,
  issueKey,
  spec,
}: {
  actions?: ReactNode;
  decision: Decision;
  issueKey: string;
  spec: Artifact | undefined;
}): ReactNode {
  return (
    <li className={`space-y-2 rounded-lg border p-3 ${card}`} data-decision={decision.block_id}>
      <h3 className={`text-[15px] font-medium ${textPrimaryOnSurface}`}>{decision.heading}</h3>
      {decisionParagraphs(decision.text).map((paragraph, index) =>
        paragraph.unsure ? (
          <div
            className={`border-l-2 pl-2 ${decisionUnsureAccent}`}
            data-testid="decision-unsure"
            // biome-ignore lint/suspicious/noArrayIndexKey: paragraphs have no id of their own
            key={index}
          >
            <MarkdownBody markdown={paragraph.markdown} />
          </div>
        ) : (
          // biome-ignore lint/suspicious/noArrayIndexKey: paragraphs have no id of their own
          <MarkdownBody key={index} markdown={paragraph.markdown} />
        )
      )}
      {decision.asks.map((ask) => (
        <Link
          className={`block text-sm ${linkText} ${linkHoverText}`}
          key={ask}
          to={buildIssuePath({ id: ask, key: issueKey, kind: "ask" })}
        >
          The ask it answers
        </Link>
      ))}
      <ShapedLinks issueKey={issueKey} links={decision.links} spec={spec} />
      {actions}
    </li>
  );
}

function WaitingDecision({
  decision,
  documentId,
  isClosed,
  issueKey,
  spec,
}: {
  decision: Decision;
  documentId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  spec: Artifact | undefined;
}): ReactNode {
  const queryClient = useQueryClient();
  const [changing, setChanging] = useState(false);
  // A change anchors on the decision's first paragraph below its heading.
  const quote = decisionParagraphs(decision.text)[0]?.markdown;
  const keep = useKeyedWrite({
    inline: true,
    label: () => `Keep ${decision.heading}`,
    write: (id: string, bounds) => api.keepDecision(id, decision.block_id, bounds),
    onSuccess: (kept) => {
      if (kept === null) {
        void queryClient.invalidateQueries({ exact: true, queryKey: ["decisions", issueKey] });
        return;
      }
      queryClient.setQueryData<IssueDecisions>(["decisions", issueKey], (view) =>
        view === undefined
          ? view
          : {
              ...view,
              decisions: view.decisions.map((row) =>
                row.block_id === kept.block_id && row.changed_version <= kept.changed_version
                  ? { ...row, kept, waiting_review: false }
                  : row
              ),
            }
      );
    },
  });
  const writable = !isClosed && documentId !== undefined;
  return (
    <DecisionCard
      actions={
        writable ? (
          <div className="space-y-2">
            <div className="flex gap-2">
              <button
                className={secondaryButtonCompact}
                disabled={keep.isPending}
                onClick={() => keep.mutate(documentId)}
                type="button"
              >
                Keep
              </button>
              {quote === undefined ? null : (
                <button
                  aria-expanded={changing}
                  className={secondaryButtonCompact}
                  onClick={() => setChanging(!changing)}
                  type="button"
                >
                  Change
                </button>
              )}
            </div>
            {keep.error === null ? null : (
              <p className={`text-sm ${dangerText}`} role="alert">
                {keep.error.message}
              </p>
            )}
            {changing && quote !== undefined ? (
              <ChangeComposer
                decision={decision}
                documentId={documentId}
                onDone={() => setChanging(false)}
                quote={quote}
              />
            ) : null}
          </div>
        ) : undefined
      }
      decision={decision}
      issueKey={issueKey}
      spec={spec}
    />
  );
}

function OpenQuestion({
  ask,
  issueKey,
  spec,
}: {
  ask: IssueAskRow;
  issueKey: string;
  spec: Artifact | undefined;
}): ReactNode {
  const path =
    ask.block_id === undefined || ask.block_id === null
      ? buildIssuePath({ id: ask.id, key: issueKey, kind: "ask" })
      : specBlockPath(issueKey, spec, ask.block_id);
  return (
    <li className={`rounded-lg border p-3 ${card}`}>
      <Link className={`text-sm ${linkText} ${linkHoverText}`} to={path}>
        {ask.question}
      </Link>
    </li>
  );
}

/** The issue's Decisions tab: its open questions, the agent's decisions waiting for a person's
 *  review (with Keep and Change), the person's own decisions, and the replaced ones, read from
 *  `GET /api/v1/issues/{key}/decisions`. Each decision links the spec blocks it shaped. */
export function DecisionsPanel({
  artifacts,
  isClosed,
  issueKey,
}: {
  artifacts: readonly Artifact[];
  isClosed: boolean;
  issueKey: string;
}): ReactNode {
  const view = useQuery({
    queryFn: () => api.getIssueDecisions(issueKey),
    queryKey: ["decisions", issueKey],
  });
  if (view.isPending) {
    return <LoadingSkeleton label="Decisions loading" />;
  }
  if (view.isError) {
    return (
      <QueryError
        message="Could not load the decisions."
        onRetry={() => void view.refetch()}
        retrying={view.isFetching}
      />
    );
  }
  const spec = artifacts.find((artifact) => artifact.primary && artifact.kind === "doc");
  const documentId = artifacts.find(
    (artifact) => artifact.slug === DECISIONS_DOCUMENT_SLUG && artifact.kind === "doc"
  )?.id;
  const { decisions, open_questions: questions } = view.data;
  const waiting = decisions.filter((decision) => decision.waiting_review);
  const mine = decisions.filter((decision) => decision.kind === "person" && !decision.replaced);
  const replaced = decisions.filter((decision) => decision.replaced);
  return (
    <div className="space-y-4">
      {view.data.document === null ? (
        <p className={`text-sm ${textMutedOnSurface}`}>This issue has no decisions.md yet.</p>
      ) : null}
      <Section empty="No open questions." title="Open questions">
        {questions.map((ask) => (
          <OpenQuestion ask={ask} issueKey={issueKey} key={ask.id} spec={spec} />
        ))}
      </Section>
      <Section empty="Nothing waits for your review." title="Waiting for your review">
        {waiting.map((decision) => (
          <WaitingDecision
            decision={decision}
            documentId={documentId}
            isClosed={isClosed}
            issueKey={issueKey}
            key={decision.block_id}
            spec={spec}
          />
        ))}
      </Section>
      <Section empty="No decisions of yours." title="Your decisions">
        {mine.map((decision) => (
          <DecisionCard
            decision={decision}
            issueKey={issueKey}
            key={decision.block_id}
            spec={spec}
          />
        ))}
      </Section>
      <Section empty="No replaced decisions." title="Replaced">
        {replaced.map((decision) => (
          <DecisionCard
            decision={decision}
            issueKey={issueKey}
            key={decision.block_id}
            spec={spec}
          />
        ))}
      </Section>
    </div>
  );
}

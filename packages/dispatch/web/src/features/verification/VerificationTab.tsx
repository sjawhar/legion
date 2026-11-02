import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { issueResultsQuery } from "../../api/queries";
import type { Artifact, CheckBlock, CheckResult } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { pillClassName } from "../../components/Pill";
import {
  card,
  dangerText,
  linkHoverText,
  linkText,
  successText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { BlockReferences } from "../refs/ReferencedBy";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildReferencePath, parseNodeReference, shortForm } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";

/**
 * How a check stands, from the newest result recorded against it. `older` and `unmarked` are
 * about the result's currency rather than its verdict: a result that no longer speaks for the
 * work as it stands says nothing about whether the check passes now, and `unknown` currency
 * means Dispatch could not read the repository to decide, which is not the same as a stale
 * result. A currency the server left out - one written before it judged currency - reads as
 * `unknown` for the same reason: nothing judged it.
 */
export type CheckState = "passed" | "failed" | "older" | "no-result" | "unmarked";

export function checkState(check: Pick<CheckBlock, "results">): CheckState {
  const latest = check.results[0];
  if (latest === undefined) {
    return "no-result";
  }
  if (latest.currency === undefined || latest.currency === "unknown") {
    return "unmarked";
  }
  if (latest.currency === "older") {
    return "older";
  }
  return latest.verdict === "pass" ? "passed" : "failed";
}

const CHECK_STATE_LABELS: Record<CheckState, string> = {
  failed: "Failed",
  "no-result": "No result",
  older: "Older",
  passed: "Passed",
  unmarked: "Unmarked",
};

const CHECK_STATE_TEXT: Record<CheckState, string> = {
  failed: dangerText,
  "no-result": textMutedOnSurface,
  older: textMutedOnSurface,
  passed: successText,
  unmarked: textMutedOnSurface,
};

/** One name/value row of a check or a result. A value the writer left blank still shows its
 *  name: that the check named no negative control is itself what a reader came to see. */
function Fact({
  children,
  label,
  testId,
}: {
  children?: ReactNode;
  label: string;
  testId: string;
}): ReactNode {
  return (
    <div className="flex flex-wrap items-baseline gap-x-2">
      <dt className={`text-xs font-medium ${textMutedOnSurface}`}>{label}</dt>
      <dd className={`text-sm ${textSecondaryOnSurface}`} data-testid={testId}>
        {children === undefined || children === "" ? "—" : children}
      </dd>
    </div>
  );
}

/** A screenshot a result names, which is a `dispatch://` reference to an image document of the
 *  issue that recorded it; one that names nothing shows as the text it is. */
function Screenshot({ reference }: { reference: string }): ReactNode {
  const route = parseNodeReference(reference);
  if (route === undefined) {
    return <span data-testid="verification-result-screenshot">{reference}</span>;
  }
  return (
    <Link
      {...referenceTriggerProps(route)}
      className={`underline ${linkText} ${linkHoverText}`}
      data-testid="verification-result-screenshot"
      to={buildReferencePath(route)}
    >
      {shortForm(route)}
    </Link>
  );
}

function Result({ result }: { result: CheckResult }): ReactNode {
  // Unknown currency draws neither mark: Dispatch could not read the repository, so saying
  // `current` or `older` would be a judgement nothing made.
  const marked = result.currency === "current" || result.currency === "older";
  return (
    <li className="space-y-2" data-testid="verification-result">
      <div className={`flex flex-wrap items-center gap-x-2 gap-y-1 text-sm ${textMutedOnSurface}`}>
        <span
          className={`${pillClassName("label")} ${result.verdict === "pass" ? successText : dangerText}`}
          data-testid="verification-result-verdict"
        >
          {result.verdict === "pass" ? "Pass" : "Fail"}
        </span>
        {marked ? (
          <span className={pillClassName("label")} data-testid="verification-result-currency">
            {result.currency}
          </span>
        ) : null}
        <span data-testid="verification-result-role">{result.role}</span>
        <span aria-hidden="true">·</span>
        <Timestamp at={result.created_at} />
      </div>
      <dl className="grid gap-x-6 gap-y-1 sm:grid-cols-2">
        <Fact label="Surface" testId="verification-result-surface">
          {result.surface}
        </Fact>
        <Fact label="Run" testId="verification-result-run">
          {result.run}
        </Fact>
        <Fact label="Observed" testId="verification-result-observed">
          {result.observed}
        </Fact>
        <Fact label="Commit" testId="verification-result-commit">
          {result.commit_sha === "" ? undefined : (
            <code title={result.commit_sha}>{result.commit_sha.slice(0, 12)}</code>
          )}
        </Fact>
        <Fact label="Negative control" testId="verification-result-negative-control">
          {result.negative_control}
        </Fact>
        <Fact label="Failed" testId="verification-result-failed">
          {String(result.failed)}
        </Fact>
        <Fact label="Skipped" testId="verification-result-skipped">
          {String(result.skipped)}
        </Fact>
        <Fact label="Excluded" testId="verification-result-excluded">
          {String(result.excluded)}
        </Fact>
      </dl>
      {result.screenshots.length === 0 ? null : (
        <div className={`flex flex-wrap items-center gap-x-2 text-sm ${textMutedOnSurface}`}>
          <span className="text-xs font-medium">Screenshots</span>
          {result.screenshots.map((reference) => (
            <Screenshot key={reference} reference={reference} />
          ))}
        </div>
      )}
    </li>
  );
}

function Check({ check }: { check: CheckBlock }): ReactNode {
  const state = checkState(check);
  return (
    <li
      className={`space-y-3 rounded-lg p-3 ${card}`}
      data-check-state={state}
      data-testid="verification-check"
    >
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <span
          className={`${pillClassName("label")} ${CHECK_STATE_TEXT[state]}`}
          data-testid="verification-check-state"
        >
          {CHECK_STATE_LABELS[state]}
        </span>
        <span className="font-medium">{check.text}</span>
      </div>
      <dl className="grid gap-x-6 gap-y-1 sm:grid-cols-2">
        <Fact label="When" testId="verification-check-when">
          {check.when}
        </Fact>
        <Fact label="Surface" testId="verification-check-surface">
          {check.surface}
        </Fact>
        <Fact label="Run" testId="verification-check-run">
          {check.run}
        </Fact>
        <Fact label="Negative control" testId="verification-check-negative-control">
          {check.negative_control}
        </Fact>
      </dl>
      <BlockReferences reference={check.ref} />
      {check.results.length === 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`} data-testid="verification-check-no-results">
          No result recorded against this check.
        </p>
      ) : (
        <ul
          aria-label={`Results of ${check.block_id}`}
          className="space-y-3"
          data-testid="verification-check-results"
        >
          {check.results.map((result) => (
            <Result key={result.id} result={result} />
          ))}
        </ul>
      )}
    </li>
  );
}

function Checks({ issueKey }: { issueKey: string }): ReactNode {
  const results = useQuery(issueResultsQuery(issueKey));

  if (results.isPending) {
    return (
      <p className={textMutedOnCanvas} data-testid="verification-loading">
        Loading checks…
      </p>
    );
  }
  if (results.isError) {
    return (
      <p className={dangerText} data-testid="verification-error">
        Could not load the checks.
      </p>
    );
  }
  if (results.data.checks.length === 0) {
    return (
      <div data-testid="verification-no-checks">
        <EmptyState
          label="Verification checks empty state"
          message="This Verification document marks no checks."
        />
      </div>
    );
  }

  return (
    <ul aria-label="Checks" className="space-y-3" data-testid="verification-checks">
      {results.data.checks.map((check) => (
        <Check check={check} key={check.block_id} />
      ))}
    </ul>
  );
}

/**
 * The issue's Verification document and, under it, each `check` block of it with the results
 * recorded against that check, newest first. `children` is the document itself, which the page
 * renders, so this panel owns only what the checks say. An issue with no Verification document
 * shows the command that makes one.
 */
export function VerificationTab({
  artifact,
  children,
  issueKey,
}: {
  artifact: Artifact | undefined;
  children?: ReactNode;
  issueKey: string;
}): ReactNode {
  if (artifact === undefined) {
    return (
      <div data-testid="verification-empty">
        <EmptyState
          action={<code>{`dispatch artifact --issue ${issueKey} --name verification.md`}</code>}
          label="Verification empty state"
          message={`${issueKey} has no Verification document. Make one with:`}
        />
      </div>
    );
  }

  return (
    <section className="space-y-4" data-testid="verification-tab">
      {children}
      <Checks issueKey={issueKey} />
    </section>
  );
}

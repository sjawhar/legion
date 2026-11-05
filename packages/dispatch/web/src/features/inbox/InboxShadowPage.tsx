import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import {
  borderDefault,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";

const causes = [
  ["ask", "Questions"],
  ["thread_reply", "Thread replies"],
  ["direct_reply", "Direct and broadcast replies"],
] as const;

export function InboxShadowPage(): ReactNode {
  useDocumentTitle("Inbox comparison · Dispatch");
  const login = useQuery(whoAmIQuery()).data?.login;
  const shadow = useQuery({
    queryKey: ["inbox-shadow", login],
    enabled: login !== undefined,
    queryFn: () => api.getInboxShadow(),
    refetchInterval: 60_000,
  });
  return (
    <section aria-label="Inbox comparison">
      <h1 className="text-xl font-semibold leading-tight tracking-tight">
        Does the new inbox miss anything?
      </h1>
      <p className={`mt-1 text-xs ${textMutedOnCanvas}`}>
        Asks and replies only. Credential requests stay in Inbox.
      </p>
      {shadow.isPending ? <LoadingSkeleton label="Comparing inboxes" /> : null}
      {shadow.isError ? (
        <p role="alert" className={dangerText}>
          Comparison unavailable: {shadow.error.message}
        </p>
      ) : null}
      {shadow.data && !shadow.isError ? (
        <>
          <p
            className={`mt-2 text-3xl font-semibold ${shadow.data.mismatched || shadow.data.errors || shadow.data.uncovered_users ? dangerText : ""}`}
            data-testid="inbox-shadow-mismatches"
          >
            {shadow.data.mismatched} differences
          </p>
          <p className="mt-1 text-sm">
            {shadow.data.uncovered_users === 0
              ? "All active people checked"
              : `${shadow.data.uncovered_users} people not checked recently`}
          </p>
          <dl className="mt-2 text-sm">
            {causes.map(([cause, label]) => (
              <div className="flex flex-wrap justify-between gap-2" key={cause}>
                <dt>{label}</dt>
                <dd>
                  {shadow.data.missing[cause] ?? 0} missing · {shadow.data.unexpected[cause] ?? 0}{" "}
                  extra
                </dd>
              </div>
            ))}
            <div className="flex justify-between gap-2">
              <dt>Comparison failures</dt>
              <dd>{shadow.data.errors}</dd>
            </div>
            <div className="flex justify-between gap-2">
              <dt>Additional mentions</dt>
              <dd>{shadow.data.expansions.mention ?? 0}</dd>
            </div>
          </dl>
          <div className={`mt-2 border-t pt-2 text-sm ${borderDefault}`}>
            <p className="font-medium">
              {shadow.data.ready
                ? "Comparison gate met; cutover still requires review"
                : "Not ready to replace your inbox"}
            </p>
            <p className={`mt-1 ${textMutedOnCanvas}`}>
              {shadow.data.complete_zero_days} of 3 complete days without differences.{" "}
              {shadow.data.daily_mismatched} differences recorded today.
            </p>
          </div>
          <p className={`mt-2 text-xs ${textMutedOnCanvas}`}>
            Last compared:{" "}
            {shadow.data.checked_at ? (
              <Timestamp at={shadow.data.checked_at} />
            ) : (
              "No completed comparison"
            )}
          </p>
          <details className="mt-1 text-sm">
            <summary className="min-h-11 cursor-pointer py-2">Your comparison and coverage</summary>
            <p className={`mb-2 ${textMutedOnCanvas}`}>
              The totals above cover everyone who has signed in, including inactive people. Only
              your own person row is shown. Counts contain no inbox content.
            </p>
            <table className="mt-2 w-full text-left">
              <thead>
                <tr>
                  <th>Person</th>
                  <th>Items</th>
                  <th>Differences</th>
                </tr>
              </thead>
              <tbody>
                {shadow.data.users.map((user) => (
                  <tr key={user.person}>
                    <td className="break-all py-2">{user.person}</td>
                    <td>{user.checked_items}</td>
                    <td>{user.mismatched}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </details>
        </>
      ) : null}
      <nav
        className={`mt-2 flex flex-wrap gap-x-4 gap-y-2 text-sm ${linkText} ${linkHoverText}`}
        aria-label="Inbox preview"
      >
        <Link className="min-h-11 py-2" to="/inbox/unified">
          Try unified inbox
        </Link>
        <Link className="min-h-11 py-2" to="/">
          Return to current inbox
        </Link>
      </nav>
    </section>
  );
}

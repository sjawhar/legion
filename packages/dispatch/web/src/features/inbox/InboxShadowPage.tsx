import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
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
  const shadow = useQuery({
    queryKey: ["inbox-shadow"],
    queryFn: () => api.getInboxShadow(),
    refetchInterval: 60_000,
  });
  const [filter, setFilter] = useState("");
  const [sort, setSort] = useState<"person" | "checked_items" | "mismatched">("person");
  const users = [...(shadow.data?.users ?? [])]
    .filter((user) => user.person.toLowerCase().includes(filter.toLowerCase()))
    .sort((a, b) =>
      sort === "person"
        ? a.person.localeCompare(b.person)
        : b[sort] - a[sort] || a.person.localeCompare(b.person)
    );
  return (
    <section aria-label="Inbox comparison">
      <h1 className="text-[22px] font-semibold tracking-tight">
        Does the new inbox miss anything?
      </h1>
      {shadow.isPending ? <LoadingSkeleton label="Comparing inboxes" /> : null}
      {shadow.isError ? (
        <p role="alert" className={dangerText}>
          Comparison unavailable: {shadow.error.message}
        </p>
      ) : null}
      {shadow.data && !shadow.isError ? (
        <>
          <p
            className={`mt-3 text-3xl font-semibold ${shadow.data.mismatched || shadow.data.errors || shadow.data.uncovered_users ? dangerText : ""}`}
            data-testid="inbox-shadow-mismatches"
          >
            {shadow.data.mismatched} differences
          </p>
          <p className="mt-1 text-sm">
            {shadow.data.uncovered_users === 0
              ? "All active people checked"
              : `${shadow.data.uncovered_users} people not checked recently`}
          </p>
          <dl className="mt-5 space-y-3 text-sm">
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
          <div className={`mt-5 border-t pt-4 text-sm ${borderDefault}`}>
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
          <p className={`mt-3 text-xs ${textMutedOnCanvas}`}>
            Last compared:{" "}
            {shadow.data.checked_at ? (
              <Timestamp at={shadow.data.checked_at} />
            ) : (
              "No completed comparison"
            )}
          </p>
          <details className="mt-4 text-sm">
            <summary className="min-h-11 cursor-pointer py-2">People and coverage</summary>
            <p className={`mb-2 ${textMutedOnCanvas}`}>
              Every person who has signed in is checked, including inactive people. Counts contain
              no inbox content.
            </p>
            <label>
              Filter people
              <input
                className={`ml-2 rounded border p-2 ${borderDefault}`}
                onChange={(event) => setFilter(event.target.value)}
                value={filter}
              />
            </label>
            <p className="mt-2">
              {users.length} of {shadow.data.users.length} people shown
            </p>
            <table className="mt-2 w-full text-left">
              <thead>
                <tr>
                  {(
                    [
                      ["person", "Person"],
                      ["checked_items", "Items"],
                      ["mismatched", "Differences"],
                    ] as const
                  ).map(([key, label]) => (
                    <th key={key}>
                      <button className="min-h-11" onClick={() => setSort(key)} type="button">
                        {label}
                      </button>
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {users.map((user) => (
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
        className={`mt-4 flex flex-wrap gap-x-4 gap-y-2 text-sm ${linkText} ${linkHoverText}`}
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

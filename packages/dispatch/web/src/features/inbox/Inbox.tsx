import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { AskCard } from "./AskCard";

export function Inbox(): ReactNode {
  const inbox = useQuery({
    queryKey: ["inbox"],
    queryFn: () => api.getInbox(),
  });

  if (inbox.isPending) {
    return <p className="text-slate-500">Loading your inbox…</p>;
  }
  if (inbox.isError) {
    return <p className="text-rose-700">Could not load your inbox.</p>;
  }
  if (inbox.data.length === 0) {
    return (
      <p className="rounded-xl border border-dashed border-slate-300 p-8 text-slate-500">
        Nothing needs you
      </p>
    );
  }

  return (
    <ul className="space-y-3">
      {inbox.data.map((ask) => (
        <li key={ask.id}>
          <Link
            className="mb-2 inline-flex items-baseline gap-2 text-sm text-sky-700 hover:text-sky-900"
            to={`/issues/${ask.issue_key}/asks/${ask.id}`}
          >
            <span className="font-semibold">{ask.issue_key}</span>
            <span>{ask.issue?.title ?? ask.issue_key}</span>
          </Link>
          <AskCard ask={ask} />
        </li>
      ))}
    </ul>
  );
}

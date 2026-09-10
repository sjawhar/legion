import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import {
  borderStrong,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { buildIssuePath } from "../refs/routes";
import { AskCard } from "./AskCard";

export function Inbox(): ReactNode {
  const inbox = useQuery({
    queryKey: ["inbox"],
    queryFn: () => api.getInbox(),
  });

  if (inbox.isPending) {
    return <p className={textMutedOnCanvas}>Loading your inbox…</p>;
  }
  if (inbox.isError) {
    return <p className={dangerText}>Could not load your inbox.</p>;
  }
  if (inbox.data.length === 0) {
    return (
      <p className={`rounded-xl border border-dashed p-8 ${borderStrong} ${textMutedOnCanvas}`}>
        Nothing needs you
      </p>
    );
  }

  return (
    <ul className="space-y-3">
      {inbox.data.map((ask) => (
        <li key={ask.id}>
          <Link
            className={`mb-2 flex flex-col items-start gap-1 text-sm md:inline-flex md:flex-row md:items-baseline md:gap-2 ${linkText} ${linkHoverText}`}
            to={buildIssuePath({ id: ask.id, key: ask.issue_key, kind: "ask" })}
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

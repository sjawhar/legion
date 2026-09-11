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
      {inbox.data.map((ask) => {
        const owner = ask.issue?.key ?? ask.issue_key;
        const documentOwner =
          ask.document === undefined ? undefined : `${ask.document.project} / ${ask.document.name}`;
        const title = ask.issue?.title ?? owner ?? documentOwner ?? "Unassigned ask";
        return (
          <li key={ask.id}>
            {owner === null ? (
              <p className={`mb-2 text-sm ${textMutedOnCanvas}`}>{title}</p>
            ) : (
              <Link
                className={`mb-2 flex flex-col items-start gap-1 text-sm md:inline-flex md:flex-row md:items-baseline md:gap-2 ${linkText} ${linkHoverText}`}
                to={buildIssuePath({ id: ask.id, key: owner, kind: "ask" })}
              >
                <span className="font-semibold">{owner}</span>
                <span>{title}</span>
              </Link>
            )}
            <AskCard ask={ask} />
          </li>
        );
      })}
    </ul>
  );
}

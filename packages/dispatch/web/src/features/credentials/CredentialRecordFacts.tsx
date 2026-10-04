import type { ReactNode } from "react";

import type { CredentialRecord } from "../../api/types";
import {
  quoteAccentBorder,
  quoteBodyText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";

const LIFETIME_UNITS: ReadonlyArray<{ seconds: number; unit: string }> = [
  { seconds: 86400, unit: "day" },
  { seconds: 3600, unit: "hour" },
  { seconds: 60, unit: "minute" },
];

/** Formats a lifetime in whole units ("15 minutes", "2 hours") rather than a raw second count. */
function formatLifetime(seconds: number): string {
  for (const { seconds: unitSeconds, unit } of LIFETIME_UNITS) {
    if (seconds >= unitSeconds) {
      const count = Math.round(seconds / unitSeconds);
      return `${count} ${unit}${count === 1 ? "" : "s"}`;
    }
  }
  return `${seconds} second${seconds === 1 ? "" : "s"}`;
}

/** The approver a request names when any signed-in person may decide it: a shared secret's. */
const ANYONE_APPROVER = "anyone";

/** One label and value; the value wraps inside its grid cell, so a 64-hex policy version or a long
 *  host name never runs into the fact beside it or widens a narrow page. */
function Fact({ children, label }: { children: ReactNode; label: string }): ReactNode {
  return (
    <div>
      <dt className={textMutedOnCanvas}>{label}</dt>
      <dd className={`break-words font-medium ${textPrimaryOnCanvas}`}>{children}</dd>
    </div>
  );
}

/**
 * The broker's facts about a credential record, then the agent's stated reason as plain text
 * (no markdown pipeline - a `<blockquote>` with `whitespace-pre-wrap` renders it verbatim), then,
 * for a pending machine (`launcher_credential`) record, the sentence explaining what approving it
 * grants. A pod enrollment's slot (`implementer-g3`) is its own fact, since several slots of one
 * pod share the same enrollment line. Shared by `CredentialRecordPage` and `MachineLoginPage`,
 * which both show this same layout before their own (page-specific) decision/action controls.
 */
export function CredentialRecordFacts({ record }: { record: CredentialRecord }): ReactNode {
  return (
    <div className="space-y-4">
      <dl className="grid grid-cols-2 gap-3 text-sm">
        <Fact label="Kind">
          {record.kind === "launcher_credential" ? "Machine login" : "Secret request"}
        </Fact>
        <Fact label="Identifiers">{record.identifiers.join(", ")}</Fact>
        <Fact label="Enrollment">
          {record.enrollment === null
            ? "—"
            : `${record.enrollment.kind} · ${record.enrollment.runtime_id} · ${record.enrollment.operator || "—"}`}
        </Fact>
        {record.enrollment?.slot ? <Fact label="Worker slot">{record.enrollment.slot}</Fact> : null}
        <Fact label="Lifetime">{formatLifetime(record.lifetime_seconds)}</Fact>
        <Fact label="Requested">
          <Timestamp at={record.requested_at} />
        </Fact>
        <Fact label="Expires">
          <Timestamp at={record.expires_at} />
        </Fact>
        <Fact label="Policy version">{record.rules_version}</Fact>
        <Fact label="Approver">
          {record.approver === ANYONE_APPROVER ? "Anyone signed in to Dispatch" : record.approver}
        </Fact>
      </dl>
      {record.reason === "" ? null : (
        <div>
          <h2 className={`text-sm font-semibold ${textMutedOnCanvas}`}>
            The agent's stated reason
          </h2>
          <blockquote
            className={`mt-1 border-l-2 pl-3 text-sm whitespace-pre-wrap ${quoteAccentBorder} ${quoteBodyText}`}
          >
            {record.reason}
          </blockquote>
        </div>
      )}
      {record.kind === "launcher_credential" && record.state === "pending" ? (
        <p className={`text-sm font-medium ${textPrimaryOnCanvas}`}>
          Approving lets {record.identifiers[0]} start agent sessions as you.
        </p>
      ) : null}
    </div>
  );
}

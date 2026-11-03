import type { ReactNode } from "react";

import type { CredentialRecord } from "../../api/types";
import {
  quoteAccentBorder,
  quoteBodyText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { machineName } from "./machineLogins";

const LIFETIME_UNITS: ReadonlyArray<{ seconds: number; unit: string }> = [
  { seconds: 86400, unit: "day" },
  { seconds: 3600, unit: "hour" },
  { seconds: 60, unit: "minute" },
];

/** Formats a lifetime in whole units ("15 minutes", "2 hours") rather than a raw second count, and
 *  no lifetime as the grant's actual end: its session's. */
function formatLifetime(seconds: number | null): string {
  if (seconds === null) {
    return "Until the session ends";
  }
  for (const { seconds: unitSeconds, unit } of LIFETIME_UNITS) {
    if (seconds >= unitSeconds) {
      const count = Math.round(seconds / unitSeconds);
      return `${count} ${unit}${count === 1 ? "" : "s"}`;
    }
  }
  return `${seconds} second${seconds === 1 ? "" : "s"}`;
}

/** The approver a record names when any signed-in person may decide it: a service's machine login
 *  here, and, on the broker's own page, a shared secret's request. */
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
 * The broker's facts about a machine login, then the agent's stated reason as plain text
 * (no markdown pipeline - a `<blockquote>` with `whitespace-pre-wrap` renders it verbatim), then,
 * for a pending one, the sentence explaining what approving it grants: a person's machine starts
 * agent sessions as them. A service (the Legion daemon) logs in with its own Kubernetes token,
 * which no person approves, so a pending service login a broker opened before that is one the
 * broker refuses to approve, and the sentence says so. `MachineLoginPage` is the one page that
 * renders this layout, above its decision controls, and the only records it holds are the machine
 * logins its code lookup returns (`POST /api/v1/credential-requests/machine-lookup`): the kind is
 * always `launcher_credential`, and the broker leaves such a record's enrollment and both of its
 * session ids empty, so neither has a fact of its own here.
 */
export function CredentialRecordFacts({ record }: { record: CredentialRecord }): ReactNode {
  return (
    <div className="space-y-4">
      <dl className="grid grid-cols-2 gap-3 text-sm">
        <Fact label="Kind">Machine login</Fact>
        <Fact label="Identifiers">{record.identifiers.join(", ")}</Fact>
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
      {record.state === "pending" ? (
        <p className={`text-sm font-medium ${textPrimaryOnCanvas}`}>
          {record.service
            ? `${machineName({ host: record.identifiers[0], service: record.service })} is a service's login from before services logged in with their own Kubernetes token, and no one can approve it any more: deny it, or let it expire.`
            : `Approving lets ${machineName({ host: record.identifiers[0], service: null })} start agent sessions as you.`}
        </p>
      ) : null}
    </div>
  );
}

import type { ReactNode } from "react";

import type { CredentialRecord } from "../../api/types";
import {
  quoteAccentBorder,
  quoteBodyText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { useAgents } from "../conversation/useAgents";
import { Timestamp } from "../refs/Timestamp";
import { CredentialSessionLines } from "./CredentialSessionLines";
import { machineName } from "./machineLogins";
import { credentialSessionNamesAnyone } from "./session";

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

/** The approver a record names when any signed-in person may decide it: a shared secret's request,
 *  and a service's machine login. */
const ANYONE_APPROVER = "anyone";

/** The three group headings' ids, which their `<section>`s name, so each group is a landmark a
 *  reader (and a screen reader) can tell from the others. */
const BROKER_HEADING_ID = "credential-broker-facts-heading";
const KERNEL_HEADING_ID = "credential-kernel-facts-heading";
const CLAIMED_HEADING_ID = "credential-claimed-facts-heading";

/** An absolute UTC instant ("2026-10-30 23:04:11Z"), the one place this page formats one: a
 *  kernel fact is read at a moment, and a relative "4 minutes ago" would not say which. An
 *  unparseable string is shown as the broker wrote it. */
function utcInstant(at: string): string {
  const date = new Date(at);
  if (Number.isNaN(date.getTime())) {
    return at;
  }
  return `${date.toISOString().slice(0, 19).replace("T", " ")}Z`;
}

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
 * A credential record's facts in three groups, each a `<section>` under its own heading, so the
 * reader reads where a value came from and not only where it sits.
 *
 * **What the broker recorded**: kind, identifiers, enrollment, a pod enrollment's slot when it
 * has one, lifetime, requested/expiry timestamps, policy version, approver - the broker's
 * `anyone`, which a shared secret's request and a service's machine login name, reads "Anyone
 * signed in to Dispatch".
 *
 * **What the kernel says about the asking process**, only when the broker answered a `peer`: the
 * executable, working directory, start and cgroup the agent-secrets helper read from the kernel
 * through the pidfd it pinned on its own socket, with the instant it read them. Its sentence
 * says what the broker checked - the session's key signed them, and on a machine session the
 * helper runs, the asking process never holds that key. The read moment and the process's start
 * are absolute UTC instants, since "4 minutes ago" would not say which moment was read. A
 * machine login, a pod (whose claim the broker refuses) and every record from before the field
 * answer no peer, and this group then renders nothing at all.
 *
 * **What the process says about itself**, always: its session, the same resolution the Inbox
 * row's own line uses, under a sentence saying nothing checked it.
 *
 * After the groups: the agent's stated reason as plain text (no markdown pipeline - a
 * `<blockquote>` with `whitespace-pre-wrap` renders it verbatim), then, for a pending machine
 * (`launcher_credential`) record, the sentence explaining what approving it grants: a person's
 * machine starts agent sessions as them. A service (the Legion daemon) logs in with its own
 * Kubernetes token, which no person approves, so a pending service login a broker opened before
 * that is one the broker refuses to approve, and the sentence says so. Shared by
 * `CredentialRecordPage` and `MachineLoginPage`, which both show this same layout before their
 * own (page-specific) decision/action controls.
 */
export function CredentialRecordFacts({ record }: { record: CredentialRecord }): ReactNode {
  const { agents, isError, isPending } = useAgents(
    credentialSessionNamesAnyone(record.session),
    true
  );
  return (
    <div className="space-y-4">
      <section aria-labelledby={BROKER_HEADING_ID}>
        <h2 className={`text-sm font-semibold ${textMutedOnCanvas}`} id={BROKER_HEADING_ID}>
          What the broker recorded
        </h2>
        <dl className="mt-2 grid grid-cols-2 gap-3 text-sm">
          <Fact label="Kind">
            {record.kind === "launcher_credential" ? "Machine login" : "Secret request"}
          </Fact>
          <Fact label="Identifiers">{record.identifiers.join(", ")}</Fact>
          <Fact label="Enrollment">
            {record.enrollment === null
              ? "—"
              : `${record.enrollment.kind} · ${record.enrollment.runtime_id} · ${record.enrollment.operator || "—"}`}
          </Fact>
          {record.enrollment?.slot ? (
            <Fact label="Worker slot">{record.enrollment.slot}</Fact>
          ) : null}
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
      </section>
      {record.peer ? (
        <section aria-labelledby={KERNEL_HEADING_ID}>
          <h2 className={`text-sm font-semibold ${textMutedOnCanvas}`} id={KERNEL_HEADING_ID}>
            What the kernel says about the asking process
          </h2>
          <p className={`mt-1 text-xs ${textMutedOnCanvas}`}>
            {`Read from the kernel through the handle the agent-secrets helper pinned, at ${utcInstant(record.peer.read_at)}, and signed with the session's key. On a machine session the helper runs, the asking process never holds that key.`}
          </p>
          <dl className="mt-2 grid grid-cols-2 gap-3 text-sm">
            <Fact label="Process ID">{record.peer.pid}</Fact>
            <Fact label="Executable">
              <span className="[overflow-wrap:anywhere]">{record.peer.executable}</span>
            </Fact>
            <Fact label="Working directory">
              <span className="[overflow-wrap:anywhere]">{record.peer.working_dir}</span>
            </Fact>
            <Fact label="Started">{utcInstant(record.peer.started_at)}</Fact>
            <Fact label="Control group">
              <span className="[overflow-wrap:anywhere]">{record.peer.cgroup}</span>
            </Fact>
          </dl>
        </section>
      ) : null}
      <section aria-labelledby={CLAIMED_HEADING_ID}>
        <h2 className={`text-sm font-semibold ${textMutedOnCanvas}`} id={CLAIMED_HEADING_ID}>
          What the process says about itself
        </h2>
        <p className={`mt-1 text-xs ${textMutedOnCanvas}`}>
          Nothing checked these. The asking process, or the launcher that started it, chose them.
        </p>
        <dl className="mt-2 grid grid-cols-2 gap-3 text-sm">
          <Fact label="Session">
            <div className="flex flex-col gap-1">
              <CredentialSessionLines
                agents={agents}
                isError={isError}
                isPending={isPending}
                session={record.session}
              />
            </div>
          </Fact>
        </dl>
      </section>
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
          {record.service
            ? `${machineName({ host: record.identifiers[0], service: record.service })} is a service's login from before services logged in with their own Kubernetes token, and no one can approve it any more: deny it, or let it expire.`
            : `Approving lets ${machineName({ host: record.identifiers[0], service: null })} start agent sessions as you.`}
        </p>
      ) : null}
    </div>
  );
}

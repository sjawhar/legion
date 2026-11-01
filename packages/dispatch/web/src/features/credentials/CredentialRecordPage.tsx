import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect } from "react";
import { Link, useParams } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { CredentialDecisionEvent } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { brokerPage } from "./brokerPage";
import { CredentialDecisionButtons } from "./CredentialDecisionButtons";
import { CredentialRecordFacts } from "./CredentialRecordFacts";
import { credentialRecordQuery } from "./record";

function CredentialDecision({ decided }: { decided: CredentialDecisionEvent }): ReactNode {
  return (
    <div className="space-y-1 text-sm">
      <p className={`font-medium first-letter:uppercase ${textPrimaryOnCanvas}`}>
        {decided.event} <Timestamp at={decided.at} />
      </p>
      {decided.credential_id === null ? null : (
        <p className={textMutedOnCanvas}>Credential {decided.credential_id}</p>
      )}
    </div>
  );
}

/**
 * The credential-record approval page: an approver reaches `/credentials/:recordId` from a link
 * made before the Inbox linked the broker, or from a shared one. A secret request is decided on
 * the secrets broker's own page now, so once the record read names that page this one replaces
 * the browser's location with it, leaving no Dispatch entry in the history to go back to. A
 * broker older than that page names none, and the page then does what it did before: it shows the
 * broker's facts and the agent's stated reason, and for a pending `agent_secret` record approves
 * or denies it with one click, Dispatch naming the viewer's own login and the broker deciding
 * whether it is the record's approver. A `launcher_credential` (machine) record never gets
 * buttons here (ruling 13 - only its typed code selects it); it links to `MachineLoginPage`
 * instead, and the broker names no page for it.
 */
export function CredentialRecordPage(): ReactNode {
  const { recordId } = useParams<{ recordId: string }>();
  const query = useQuery(credentialRecordQuery(recordId ?? ""));
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();

  const invalidateAfterDecision = () => {
    void queryClient.invalidateQueries({ queryKey: ["credential-record", recordId] });
    void queryClient.invalidateQueries({ queryKey: ["credential-pending"] });
  };

  // The decision's own buttons show a refusal, beside the facts it was refused over.
  const approve = useKeyedWrite({
    inline: true,
    label: () => "Approval of the credential request",
    onSettled: () => submitGuard.release(),
    onSuccess: invalidateAfterDecision,
    write: (_variables, bounds) => api.approveCredentialRecord(recordId ?? "", {}, bounds),
  });
  const deny = useKeyedWrite({
    inline: true,
    label: () => "Denial of the credential request",
    onSettled: () => submitGuard.release(),
    onSuccess: invalidateAfterDecision,
    write: (_variables, bounds) => api.denyCredentialRecord(recordId ?? "", {}, bounds),
  });

  // The forward runs in an effect rather than during the render, because it leaves the
  // application: replacing the location is a side effect, and a render that performed one would
  // run it again on every re-render the query makes. `replace` rather than an assignment, so the
  // back button returns to wherever the person came from rather than to this page, which would
  // forward them again.
  const page = brokerPage(query.data?.approve_url);
  useEffect(() => {
    if (page !== null) {
      window.location.replace(page);
    }
  }, [page]);

  useDocumentTitle("Credential request · Dispatch");

  if (recordId === undefined) {
    return null;
  }
  if (query.isPending) {
    return <p className={textMutedOnCanvas}>Loading credential request…</p>;
  }
  if (query.isError) {
    if (query.error instanceof ApiError && query.error.status === 404) {
      return <p className={dangerText}>This credential request no longer exists.</p>;
    }
    return (
      <QueryError
        message="Could not load this credential request."
        onRetry={() => void query.refetch()}
      />
    );
  }

  // The effect above is already sending the browser to the broker; this is what a person sees if
  // the browser has not left yet, and the link is what they follow if it never does.
  if (page !== null) {
    return (
      <p className={textMutedOnCanvas}>
        This request is decided on the secrets broker.{" "}
        <a className={`underline ${linkText} ${linkHoverText}`} href={page}>
          Open its page there
        </a>
        .
      </p>
    );
  }

  const record = query.data;

  return (
    <section className="max-w-2xl space-y-6">
      <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>Credential request</h1>
      <CredentialRecordFacts record={record} />
      {record.kind === "launcher_credential" ? (
        <Link
          className={`inline-flex min-h-11 items-center text-sm font-medium underline ${linkText} ${linkHoverText}`}
          to="/credentials/machine"
        >
          Enter the code shown on the machine
        </Link>
      ) : record.state === "pending" ? (
        <CredentialDecisionButtons approve={approve} deny={deny} submitGuard={submitGuard} />
      ) : record.decided !== null ? (
        <CredentialDecision decided={record.decided} />
      ) : null}
    </section>
  );
}

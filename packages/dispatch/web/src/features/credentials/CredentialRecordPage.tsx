import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
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
 * The credential-record approval page: an approver reaches `/credentials/:recordId` from the
 * inbox's credential-requests section (or a shared link), sees the broker's facts and the
 * agent's stated reason, and, for a pending `agent_secret` record, approves or denies it with one
 * click — Dispatch names the viewer's own login, and the broker decides whether it is the
 * record's approver. A `launcher_credential` (machine) record never gets buttons here (ruling
 * 13 - only its typed code selects it); it links to `MachineLoginPage` instead.
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

  const approve = useMutation({
    mutationFn: () => api.approveCredentialRecord(recordId ?? ""),
    onSettled: () => submitGuard.release(),
    onSuccess: invalidateAfterDecision,
  });
  const deny = useMutation({
    mutationFn: () => api.denyCredentialRecord(recordId ?? ""),
    onSettled: () => submitGuard.release(),
    onSuccess: invalidateAfterDecision,
  });

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

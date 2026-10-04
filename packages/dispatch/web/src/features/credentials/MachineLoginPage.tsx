import { useMutation, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  dangerText,
  inputClasses,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { CredentialDecisionButtons } from "./CredentialDecisionButtons";
import { CredentialRecordFacts } from "./CredentialRecordFacts";
import { MachineLoginsSection } from "./MachineLoginsSection";
import { machineLoginsQuery, machineName } from "./machineLogins";

/** What the login's decision was, in place of the buttons that decide it: one just made on this
 *  page, or the one a looked-up record already carries. A service's approved login (the Legion
 *  daemon's) starts worker pods as the service, where a person's machine starts sessions as them. */
function MachineLoginDecision({
  credentialId,
  event,
  host,
  service,
}: {
  credentialId: string | null;
  event: string;
  host: string;
  service: string | null;
}): ReactNode {
  return (
    <div className="space-y-1 text-sm">
      <p className={`font-medium ${textPrimaryOnCanvas}`}>
        {event !== "approved" ? (
          <>
            <span className="capitalize">{event}</span>. {host} is not logged in.
          </>
        ) : (
          `Approved. ${machineName({ host, service })} can start ${service ? `worker pods as ${service}` : "agent sessions as you"}.`
        )}
      </p>
      {credentialId === null ? null : (
        <p className={textMutedOnCanvas}>Credential {credentialId}</p>
      )}
    </div>
  );
}

/**
 * The machine-login page: `agent-secrets launcher login` prints an 8-character code on the
 * machine, and the operator types it here. Ruling 13 of the shared broker contract
 * (dispatch://AGENTC-393/artifact/plan-overview-md): only this code-lookup route selects a
 * `launcher_credential` record, and deciding it sends the same code again, so this is the one
 * place a machine record gets Approve/Deny buttons. A looked-up login already decided shows its
 * decision, and so does one decided here, in their place, as the record page does, so a second
 * click never reaches the broker's already-decided refusal. Below it, the viewer's machine logins
 * that can still reach a secret, each revocable.
 */
export function MachineLoginPage(): ReactNode {
  const [code, setCode] = useState("");
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();

  const lookup = useMutation({
    mutationFn: (lookupCode: string) => api.lookupMachineCredential(lookupCode),
    onSettled: () => submitGuard.release(),
  });
  const record = lookup.data;
  // The code the shown record was looked up by, not whatever the field holds now.
  const lookedUpCode = lookup.variables ?? "";

  const approve = useMutation({
    mutationFn: () => api.approveCredentialRecord(record?.record_id ?? "", { code: lookedUpCode }),
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["credential-pending"] });
      void queryClient.invalidateQueries({ queryKey: machineLoginsQuery().queryKey });
    },
  });
  const deny = useMutation({
    mutationFn: () => api.denyCredentialRecord(record?.record_id ?? "", { code: lookedUpCode }),
    onSettled: () => submitGuard.release(),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["credential-pending"] }),
  });

  useDocumentTitle("Machine login · Dispatch");

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    submitGuard.guard(() => {
      approve.reset();
      deny.reset();
      lookup.mutate(code);
    });
  };
  const madeHere = approve.data ?? deny.data;
  // The looked-up record and its decision as the page now knows them: a decision made here leaves
  // the record decided, with the credential an approval minted.
  const view =
    record === undefined
      ? undefined
      : madeHere === undefined
        ? { decided: record.decided, record }
        : {
            decided: {
              credential_id: madeHere.state === "approved" ? madeHere.credential_id : null,
              event: madeHere.state,
            },
            record: { ...record, state: madeHere.state },
          };

  return (
    <section className="max-w-2xl space-y-6">
      <h1 className={`text-xl font-semibold ${textPrimaryOnCanvas}`}>Enter machine login code</h1>
      <form className="flex flex-wrap items-end gap-3" onSubmit={submit}>
        <div className="flex flex-col gap-1">
          <label className={`text-sm font-medium ${textMutedOnCanvas}`} htmlFor="machine-code">
            Code shown on the machine
          </label>
          <input
            className={`rounded-lg border px-3 py-2 font-mono ${inputClasses(false)} ${textPrimaryOnCanvas}`}
            id="machine-code"
            onChange={(event) => {
              const cleaned = event.target.value
                .toUpperCase()
                .replaceAll(/[^A-Z0-9]/g, "")
                .slice(0, 8);
              setCode(cleaned.length > 4 ? `${cleaned.slice(0, 4)}-${cleaned.slice(4)}` : cleaned);
            }}
            placeholder="XXXX-XXXX"
            value={code}
          />
        </div>
        <button
          className={`min-h-11 rounded-lg px-4 py-2 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
          disabled={lookup.isPending || code.trim() === ""}
          type="submit"
        >
          Look up
        </button>
      </form>
      {lookup.isError ? (
        <p className={dangerText}>
          {apiErrorMessage(lookup.error, "Could not look up that code.")}
        </p>
      ) : null}
      {view === undefined ? null : (
        <div className="space-y-4">
          <CredentialRecordFacts record={view.record} />
          {view.decided === null ? (
            <CredentialDecisionButtons approve={approve} deny={deny} submitGuard={submitGuard} />
          ) : (
            <MachineLoginDecision
              credentialId={view.decided.credential_id}
              event={view.decided.event}
              host={view.record.identifiers[0] ?? ""}
              service={view.record.service}
            />
          )}
        </div>
      )}
      <MachineLoginsSection />
    </section>
  );
}

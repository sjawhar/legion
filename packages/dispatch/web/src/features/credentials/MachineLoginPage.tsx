import { useMutation, useQueryClient } from "@tanstack/react-query";
import { type FormEvent, type ReactNode, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { getAssertion } from "../../lib/webauthn";
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

/**
 * The machine-login code-entry page: `agent-secrets launcher login` prints an 8-character code
 * on the machine, and the operator types it here. Contract v9 ruling 13 - a `launcher_credential`
 * record's WebAuthn challenges only ever come from this code-lookup route, never from
 * `getCredentialRecord`, so this is the one place a machine record gets Approve/Deny buttons.
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

  const approve = useMutation({
    mutationFn: async () => {
      if (record === undefined || record.challenges === null) {
        throw new Error("No approve challenge available for this record");
      }
      const assertion = await getAssertion(record.challenges.approve, window.location.hostname);
      return api.approveCredentialRecord(record.record_id, { assertion, code });
    },
    onSettled: () => submitGuard.release(),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["credential-pending"] }),
  });
  const deny = useMutation({
    mutationFn: async () => {
      if (record === undefined || record.challenges === null) {
        throw new Error("No deny challenge available for this record");
      }
      const assertion = await getAssertion(record.challenges.deny, window.location.hostname);
      return api.denyCredentialRecord(record.record_id, { assertion });
    },
    onSettled: () => submitGuard.release(),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["credential-pending"] }),
  });

  useDocumentTitle("Machine login · Dispatch");

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    submitGuard.guard(() => lookup.mutate(code));
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
      {record === undefined ? null : (
        <div className="space-y-4">
          <CredentialRecordFacts record={record} />
          {record.state === "pending" && record.challenges !== null ? (
            <CredentialDecisionButtons approve={approve} deny={deny} submitGuard={submitGuard} />
          ) : null}
        </div>
      )}
    </section>
  );
}

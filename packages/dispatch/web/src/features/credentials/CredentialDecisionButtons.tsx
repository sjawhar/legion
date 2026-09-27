import type { UseMutationResult } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { apiErrorMessage } from "../../api/client";
import type { CredentialDecisionResponse } from "../../api/types";
import type { SubmitGuard } from "../../hooks/useSubmitGuard";
import {
  dangerText,
  primaryButtonBg,
  primaryButtonDisabled,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";

/**
 * The Approve/Deny button pair both `CredentialRecordPage` and `MachineLoginPage` render for a
 * pending record with challenges: each page builds its own `approve`/`deny` mutations (the POST
 * body differs - `MachineLoginPage`'s approve carries the typed code, `CredentialRecordPage`'s
 * doesn't), then hands the mutations here for the shared disabled-guard, click-wiring, and
 * verbatim broker-error rendering.
 */
export function CredentialDecisionButtons({
  approve,
  deny,
  submitGuard,
}: {
  approve: UseMutationResult<CredentialDecisionResponse, Error, void>;
  deny: UseMutationResult<CredentialDecisionResponse, Error, void>;
  submitGuard: SubmitGuard;
}): ReactNode {
  return (
    <div className="space-y-2">
      <div className="flex gap-2">
        <button
          className={`min-h-11 rounded-lg px-4 py-2 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
          disabled={approve.isPending || deny.isPending}
          onClick={() => submitGuard.guard(() => approve.mutate())}
          type="button"
        >
          Approve
        </button>
        <button
          className={`min-h-11 rounded-lg border px-4 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          disabled={approve.isPending || deny.isPending}
          onClick={() => submitGuard.guard(() => deny.mutate())}
          type="button"
        >
          Deny
        </button>
      </div>
      {approve.isError ? (
        <p className={dangerText}>
          {apiErrorMessage(approve.error, "Could not approve this request.")}
        </p>
      ) : null}
      {deny.isError ? (
        <p className={dangerText}>{apiErrorMessage(deny.error, "Could not deny this request.")}</p>
      ) : null}
    </div>
  );
}

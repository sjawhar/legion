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
 * The Approve/Deny button pair `MachineLoginPage` renders for a pending machine login: the page
 * builds its own `approve`/`deny` mutations, whose POST bodies carry the typed code, then hands
 * them here for the shared disabled-guard, click-wiring, and verbatim broker-error rendering. With
 * no `approve`, only Deny shows: a record nobody may approve any more, such as a service's machine
 * login from before services logged in with their tokens.
 */
export function CredentialDecisionButtons({
  approve,
  deny,
  submitGuard,
}: {
  approve?: UseMutationResult<CredentialDecisionResponse, Error, void>;
  deny: UseMutationResult<CredentialDecisionResponse, Error, void>;
  submitGuard: SubmitGuard;
}): ReactNode {
  const deciding = approve?.isPending === true || deny.isPending;
  return (
    <div className="space-y-2">
      <div className="flex gap-2">
        {approve === undefined ? null : (
          <button
            className={`min-h-11 rounded-lg px-4 py-2 text-sm font-medium ${primaryButtonBg} ${primaryButtonEnabledHoverBg} ${primaryButtonDisabled}`}
            disabled={deciding}
            onClick={() => submitGuard.guard(() => approve.mutate())}
            type="button"
          >
            Approve
          </button>
        )}
        <button
          className={`min-h-11 rounded-lg border px-4 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          disabled={deciding}
          onClick={() => submitGuard.guard(() => deny.mutate())}
          type="button"
        >
          Deny
        </button>
      </div>
      {approve?.isError ? (
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

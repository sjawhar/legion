import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  card,
  dangerText,
  hoverToDangerText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { Timestamp } from "../refs/Timestamp";
import { credentialGrantsQuery } from "./grants";

/**
 * The live approval-granted credential grants the viewer approved, and those on enrollments the
 * viewer operates whoever approved them, each naming its approver and revocable with one click:
 * Dispatch sends the broker the viewer's own login, and the broker allows the revoke only when that
 * login is the grant's approver or its enrollment's operator. Rendered on the Settings page.
 */
export function GrantsSection(): ReactNode {
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const grants = useQuery(credentialGrantsQuery());
  const revoke = useMutation({
    mutationFn: (grantId: string) => api.revokeCredentialGrant(grantId),
    onSettled: () => submitGuard.release(),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: credentialGrantsQuery().queryKey }),
  });

  return (
    <section aria-labelledby="credential-grants-heading" className="mt-10">
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="credential-grants-heading">
        Live grants
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Live credential grants you approved, and those on enrollments you operate, whoever approved
        them. Revoking one ends its access immediately.
      </p>

      {grants.isPending ? <p className={`mt-6 ${textMutedOnCanvas}`}>Loading grants…</p> : null}
      {grants.isError ? (
        <div className="mt-6">
          <QueryError
            message="Couldn't load grants."
            onRetry={() => void grants.refetch()}
            retrying={grants.isFetching}
          />
        </div>
      ) : null}
      {grants.isSuccess ? (
        <div className={`mt-6 overflow-x-auto rounded-xl border ${card}`}>
          <table className="w-full text-left text-sm">
            <thead className={`border-b ${textSecondaryOnSurface}`}>
              <tr>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Enrollment
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Names
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Approver
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Created
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Expires
                </th>
                <th className="px-4 py-3" scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {grants.data.grants.length === 0 ? (
                <tr>
                  <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={6}>
                    No live grants.
                  </td>
                </tr>
              ) : (
                grants.data.grants.map((grant) => (
                  <tr className="border-b last:border-0" key={grant.grant_id}>
                    <td className={`px-4 py-3 font-medium ${textPrimaryOnSurface}`}>
                      {grant.enrollment.kind} · {grant.enrollment.runtime_id} ·{" "}
                      {grant.enrollment.operator ?? "—"}
                    </td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                      {grant.names.join(", ")}
                    </td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>{grant.approver}</td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                      <Timestamp at={grant.created_at} />
                    </td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                      <Timestamp at={grant.expires_at} />
                    </td>
                    <td className="px-4 py-3 text-right">
                      <button
                        className={`min-h-11 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap disabled:cursor-not-allowed disabled:opacity-50 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText} ${hoverToDangerText}`}
                        disabled={revoke.isPending}
                        onClick={() => submitGuard.guard(() => revoke.mutate(grant.grant_id))}
                        type="button"
                      >
                        Revoke
                      </button>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      ) : null}
      {revoke.isError ? (
        <p className={`mt-2 ${dangerText}`}>
          {apiErrorMessage(revoke.error, "Could not revoke this grant.")}
        </p>
      ) : null}
    </section>
  );
}

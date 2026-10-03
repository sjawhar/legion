import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api, apiErrorMessage } from "../../api/client";
import type { CredentialGrant } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
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
import { settingsTableWrapper } from "../settings/classes";
import { credentialGrantsQuery } from "./grants";

/** How a grant came to be, for its Live grants row: the policy's, or the person who approved it. */
function grantedBy(grant: CredentialGrant): string {
  return grant.granted === "automatic" ? "Automatically" : `Approved by ${grant.approver ?? "—"}`;
}

/**
 * The viewer's live credential grants: every grant of a session the viewer operates, whether the
 * policy gave it without asking or someone approved it, and every grant the viewer approved on
 * anyone's session. Each row names how it was granted and is revocable with one click: Dispatch
 * sends the broker the viewer's own login, and the broker allows the revoke only when that login
 * is the grant's approver or its enrollment's operator. Revoking an automatic grant also makes
 * that session ask before it gets those secrets again. A pod enrollment's slot shows under its
 * enrollment, so the grants of two roles in one pod read apart. Rendered on the Settings page.
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
        Every live grant of your sessions, given automatically or on approval, and the grants you
        approved. Revoking one ends its access immediately; after you revoke an automatic grant,
        that session asks before it gets those secrets again.
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
        <div className={settingsTableWrapper}>
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
                  Granted
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
                      {grant.enrollment.operator || "—"}
                      {grant.enrollment.slot ? (
                        <span className={`block text-xs font-normal ${textSecondaryOnSurface}`}>
                          slot {grant.enrollment.slot}
                        </span>
                      ) : null}
                    </td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                      {grant.names.join(", ")}
                    </td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>{grantedBy(grant)}</td>
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

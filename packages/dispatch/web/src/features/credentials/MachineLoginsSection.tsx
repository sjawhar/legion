import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api, apiErrorMessage } from "../../api/client";
import type { MachineLogin } from "../../api/types";
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

/** The viewer's machine logins, which approving one on the machine-login page and revoking one
 *  here both invalidate. */
export const machineLoginsQueryKey = ["machine-logins"] as const;

/**
 * The live machine logins the viewer approved: one row per machine logged in as them, and one per
 * service whose login they approved (the Legion daemon's, labelled `legion-daemon on <host>`), with
 * when it was issued and when it expires. Revoke ends a login before then: it enrolls no more
 * sessions, and every session it enrolled (a service's worker pods) loses the broker at once, its
 * grants with it. Dispatch names the viewer as the person revoking, and the broker refuses anyone
 * but the login's approver. Rendered on the machine-login page.
 */
export function MachineLoginsSection(): ReactNode {
  const queryClient = useQueryClient();
  const submitGuard = useSubmitGuard();
  const logins = useQuery({
    queryKey: machineLoginsQueryKey,
    queryFn: () => api.getMachineLogins(),
  });
  const revoke = useMutation({
    mutationFn: (credentialId: string) => api.revokeMachineLogin(credentialId),
    onSettled: () => submitGuard.release(),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: machineLoginsQueryKey });
      void queryClient.invalidateQueries({ queryKey: credentialGrantsQuery().queryKey });
    },
  });
  const confirmRevoke = (login: MachineLogin) => {
    const question = login.service
      ? `Revoke the ${login.service} login on ${login.host}? Every session it started, its worker pods included, ends at once, and ${login.service} needs a new login approval before it starts any more.`
      : `Revoke the machine login for ${login.host}? Every agent session it started loses its secrets at once, and the machine needs a new login.`;
    if (window.confirm(question)) {
      submitGuard.guard(() => revoke.mutate(login.credential_id));
    }
  };

  return (
    <section aria-labelledby="machine-logins-heading" className="pt-4">
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id="machine-logins-heading">
        Your machine logins
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>
        Every machine logged in as you, and every service whose login you allowed, until its login
        expires. Revoking one ends it now: it starts no more sessions, and every session it started
        loses its secrets.
      </p>

      {logins.isPending ? (
        <p className={`mt-6 ${textMutedOnCanvas}`}>Loading machine logins…</p>
      ) : null}
      {logins.isError ? (
        <div className="mt-6">
          <QueryError
            message="Couldn't load machine logins."
            onRetry={() => void logins.refetch()}
            retrying={logins.isFetching}
          />
        </div>
      ) : null}
      {logins.isSuccess ? (
        <div className={settingsTableWrapper}>
          <table className="w-full text-left text-sm">
            <thead className={`border-b ${textSecondaryOnSurface}`}>
              <tr>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Machine
                </th>
                <th className="px-4 py-3 font-semibold" scope="col">
                  Issued
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
              {logins.data.credentials.length === 0 ? (
                <tr>
                  <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={4}>
                    No live machine logins.
                  </td>
                </tr>
              ) : (
                logins.data.credentials.map((login) => (
                  <tr className="border-b last:border-0" key={login.credential_id}>
                    <td className={`px-4 py-3 font-medium ${textPrimaryOnSurface}`}>
                      {login.service ? `${login.service} on ${login.host}` : login.host}
                    </td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                      <Timestamp at={login.issued_at} />
                    </td>
                    <td className={`px-4 py-3 ${textSecondaryOnSurface}`}>
                      <Timestamp at={login.expires_at} />
                    </td>
                    <td className="px-4 py-3 text-right">
                      <button
                        className={`min-h-11 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap disabled:cursor-not-allowed disabled:opacity-50 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText} ${hoverToDangerText}`}
                        disabled={revoke.isPending}
                        onClick={() => confirmRevoke(login)}
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
          {apiErrorMessage(revoke.error, "Could not revoke this machine login.")}
        </p>
      ) : null}
    </section>
  );
}

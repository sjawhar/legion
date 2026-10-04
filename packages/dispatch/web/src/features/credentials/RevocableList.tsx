import { type UseQueryResult, useMutation } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { apiErrorMessage } from "../../api/client";
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
import { settingsTableWrapper } from "../settings/classes";

/** One column of a `RevocableList`: its header, and what a row shows under it. The first column
 *  names the row. */
export interface RevocableColumn<Row> {
  cell: (row: Row) => ReactNode;
  header: string;
}

/**
 * The revocable table `GrantsSection` and `MachineLoginsSection` both render: a heading and what
 * the list holds, the list's loading, failed and listed states, one row per item with a Revoke
 * button, and a refused revoke's broker message under the table (a `NOT_APPROVER` 403 among them),
 * so no refusal is silent. Each section hands over its own query, columns and revoke call, and
 * `confirm` when a revoke asks first; the list owns the one revoke in flight: every Revoke button
 * is disabled while it is pending, and the submit guard drops a second press that lands before
 * that re-render (a same-tick double click). `onRevoked` runs once a revoke succeeds, for the
 * section to refresh what it changed.
 */
export function RevocableList<Row>({
  className,
  columns,
  confirm,
  description,
  empty,
  heading,
  headingId,
  noun,
  onRevoked,
  query,
  revoke,
  revokeError,
  rowKey,
}: {
  className: string;
  columns: readonly RevocableColumn<Row>[];
  confirm?: (row: Row) => string;
  description: ReactNode;
  empty: string;
  heading: string;
  headingId: string;
  noun: string;
  onRevoked: () => void;
  query: UseQueryResult<readonly Row[]>;
  revoke: (row: Row) => Promise<void>;
  revokeError: string;
  rowKey: (row: Row) => string;
}): ReactNode {
  const submitGuard = useSubmitGuard();
  const mutation = useMutation({
    mutationFn: revoke,
    onSettled: () => submitGuard.release(),
    onSuccess: onRevoked,
  });
  const press = (row: Row) => {
    if (confirm === undefined || window.confirm(confirm(row))) {
      submitGuard.guard(() => mutation.mutate(row));
    }
  };

  return (
    <section aria-labelledby={headingId} className={className}>
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} id={headingId}>
        {heading}
      </h2>
      <p className={`mt-1 text-sm ${textSecondaryOnCanvas}`}>{description}</p>

      {query.isPending ? <p className={`mt-6 ${textMutedOnCanvas}`}>Loading {noun}…</p> : null}
      {query.isError ? (
        <div className="mt-6">
          <QueryError
            message={`Couldn't load ${noun}.`}
            onRetry={() => void query.refetch()}
            retrying={query.isFetching}
          />
        </div>
      ) : null}
      {query.isSuccess ? (
        <div className={settingsTableWrapper}>
          <table className="w-full text-left text-sm">
            <thead className={`border-b ${textSecondaryOnSurface}`}>
              <tr>
                {columns.map((column) => (
                  <th className="px-4 py-3 font-semibold" key={column.header} scope="col">
                    {column.header}
                  </th>
                ))}
                <th className="px-4 py-3" scope="col">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {query.data.length === 0 ? (
                <tr>
                  <td className={`px-4 py-5 ${textMutedOnSurface}`} colSpan={columns.length + 1}>
                    {empty}
                  </td>
                </tr>
              ) : (
                query.data.map((row) => (
                  <tr className="border-b last:border-0" key={rowKey(row)}>
                    {columns.map((column, index) => (
                      <td
                        className={
                          index === 0
                            ? `px-4 py-3 font-medium ${textPrimaryOnSurface}`
                            : `px-4 py-3 ${textSecondaryOnSurface}`
                        }
                        key={column.header}
                      >
                        {column.cell(row)}
                      </td>
                    ))}
                    <td className="px-4 py-3 text-right">
                      <button
                        className={`min-h-11 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap disabled:cursor-not-allowed disabled:opacity-50 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText} ${hoverToDangerText}`}
                        disabled={mutation.isPending}
                        onClick={() => press(row)}
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
      {mutation.isError ? (
        <p className={`mt-2 ${dangerText}`}>{apiErrorMessage(mutation.error, revokeError)}</p>
      ) : null}
    </section>
  );
}

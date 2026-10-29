import type { ReactNode } from "react";

import { dangerText, secondaryButtonCompact, textSecondaryOnSurface } from "../../theme/classes";
import { useWriteSelection, useWrites, type Write } from "./write-store";

/** The writes a signed-out page still keeps for their person: every one they can see that
 *  Dispatch has not made. A read mark is never shown. */
function keptWrites(rows: readonly Write[]): readonly Write[] {
  return rows.filter(
    (row) => row.login !== undefined && row.presentation !== "quiet" && row.status !== "made"
  );
}

/**
 * What a lapsed session keeps, on the sign-in page: the person's writes by name, under the login
 * whose sign-in takes them up again (`Writes.signedIn`), so they see what waits for them; one
 * Dispatch refused offers Discard, since only they can drop it. Nothing while the page keeps none,
 * as after a sign-out.
 */
export function KeptWrites(): ReactNode {
  const writes = useWrites();
  const rows = useWriteSelection("kept-while-signed-out", keptWrites);
  if (rows.length === 0) return null;
  const logins = [...new Set(rows.map((row) => row.login))];
  return (
    <section aria-label="Kept changes" className="mt-6 space-y-3 text-sm">
      {logins.map((login) => (
        <div key={login}>
          <p className={textSecondaryOnSurface}>Kept until you sign in again as {login}:</p>
          <ul className="mt-2 space-y-1">
            {rows
              .filter((row) => row.login === login)
              .map((row) => (
                <li
                  className="flex min-h-8 flex-wrap items-center gap-x-2 gap-y-1"
                  data-kept-write={row.id}
                  key={row.id}
                >
                  {row.status === "refused" ? (
                    <>
                      <span className={`min-w-0 flex-1 ${dangerText}`}>
                        {row.label} — Not saved
                      </span>
                      <button
                        className={secondaryButtonCompact}
                        onClick={() => writes.discard(row.id)}
                        type="button"
                      >
                        Discard
                      </button>
                    </>
                  ) : (
                    <span className="min-w-0 flex-1">{row.label}</span>
                  )}
                </li>
              ))}
          </ul>
        </div>
      ))}
    </section>
  );
}

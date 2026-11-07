import { textMutedOnSurface } from "../../theme/classes";
import { type Attempt, plainWrite, type Writes } from "../writes/write-store";

/** The quiet line the bar says under its list: what an offer is writing, what it wrote, what it
 *  could not write, and how many more matches the search found than the list holds. */
export const barNoteClassName = `shrink-0 px-3 py-2 text-xs ${textMutedOnSurface}`;

/** One request, under a key of its own, tried until Dispatch decides it and named in the shell's
 *  indicator while it is out. A filing is four of these rather than one, because one write
 *  makes one request (`OneRequestPerWriteError`): Dispatch would refuse a second request under a
 *  key it has already decided. `inline` because the bar says a refusal itself. */
export function press<T>(writes: Writes, label: string, write: Attempt<T>): Promise<T | null> {
  return writes.run({
    detail: null,
    inline: true,
    kind: plainWrite,
    label,
    presentation: "indicator",
    write,
  });
}

/** Dispatch's own refusal where it gave one, and `whenSilent` for whatever else stopped the
 *  write: a reader told only "could not" has nothing to act on. */
export function refusalText(error: unknown, whenSilent: string): string {
  return error instanceof Error && error.message !== "" ? error.message : whenSilent;
}

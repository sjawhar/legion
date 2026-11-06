import type { ReactNode } from "react";

import {
  borderDefault,
  borderStrong,
  borderTransparent,
  linkText,
  skeletonBg,
  textTransparent,
} from "../../theme/classes";

/**
 * The two pieces of the issue page a reader has before the page itself: the header's box and the
 * tab strip's.
 *
 * They are drawn twice, and the same both times. `IssuePageFallback` (`app.tsx`) draws them while
 * the page's chunk is still arriving, and the page draws them again around its header and its
 * tabs, each of which arrives in a commit of its own now rather than in the page's one mount
 * commit (`IssuePage.tsx`, dispatch://INFRA-341 R11). So nothing the reader has not already seen
 * appears between the skeleton and the page, and the document under them does not move as each
 * part lands.
 *
 * Here rather than in `app.tsx` because both draw them: `app.tsx` is the entry chunk, which may
 * hold nothing a route draws, and these two boxes are all of the page that belongs there.
 */
export function IssueHeaderSkeleton(): ReactNode {
  return (
    <header className={`mb-6 border-b pb-6 ${borderDefault}`}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className={`text-sm font-semibold ${linkText}`}>
          <span
            className={`inline-block h-4 w-16 animate-pulse rounded align-middle ${skeletonBg}`}
          />
        </p>
        <button
          className={`rounded-lg border px-3 py-2 text-sm font-medium ${textTransparent} ${borderStrong}`}
          disabled
          type="button"
        >
          Pin issue
        </button>
      </div>
      <h1
        className={`mt-2 w-full rounded-lg border px-2 py-1 text-2xl font-semibold ${borderTransparent}`}
      >
        <span
          className={`inline-block h-7 w-2/3 animate-pulse rounded align-middle ${skeletonBg}`}
        />
      </h1>
    </header>
  );
}

/** The tab strip's box, with the tabs the page opens with named. */
export function IssueTabsSkeleton(): ReactNode {
  return (
    <div className={`mb-4 flex gap-2 border-b ${borderDefault}`} role="tablist">
      {["Spec", "Conversation", "Children", "Artifacts", "Activity"].map((label) => (
        <span
          className={`animate-pulse rounded px-3 py-2 text-sm ${textTransparent} ${skeletonBg}`}
          key={label}
        >
          {label}
        </span>
      ))}
    </div>
  );
}

import type { ReactNode } from "react";

import { tabClassName, tabListClassName, tabStripClassName } from "../../components/tab-classes";
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
 * commit (`IssuePage.tsx`, dispatch://INFRA-341 R11). On a document address that commit waits
 * for the editor's first batch, so the box each keeps is the box a reader reads for seconds
 * rather than for a frame (`features/doc/AfterTheFirstBatch.tsx`). So nothing the reader has not
 * already seen appears between the skeleton and the page.
 *
 * Here rather than in `app.tsx` because both draw them: `app.tsx` is the entry chunk, which may
 * hold nothing a route draws, and these two boxes are all of the page that belongs there.
 */
export function IssueHeaderSkeleton(): ReactNode {
  return (
    <header className={`mb-6 border-b pb-6 ${borderDefault}`} data-testid="issue-header-box">
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

/** The tabs the strip names, in the order `IssueTabs` lists them. Here as plain text rather
 *  than read from that module: the tabs are a chunk of their own, and a static import of it
 *  would put the strip back in the commit this box exists to keep it out of. */
const ISSUE_TABS = [
  "Spec",
  "Verification",
  "Plan",
  "Conversation",
  "Children",
  "Artifacts",
  "Activity",
  "Decisions",
] as const;

/**
 * The tab strip's box, at the strip's own size: the same classes `Tabs` draws itself with
 * (`components/tab-classes.ts`) around a box per tab the issue has, so the strip landing in it
 * moves the document under it by no pixels.
 *
 * Each tab is a pulsing box the width of its own name rather than a tab that reads as one: the
 * strip is what the reader presses, and nothing here answers a press or a screen reader.
 */
export function IssueTabsSkeleton(): ReactNode {
  return (
    <div aria-hidden="true" className={tabStripClassName(true)} data-testid="issue-tabs-box">
      <div className={tabListClassName(true)}>
        {ISSUE_TABS.map((label) => (
          <span className={tabClassName(true, false)} key={label}>
            <span className={`animate-pulse rounded ${textTransparent} ${skeletonBg}`}>
              {label}
            </span>
          </span>
        ))}
      </div>
    </div>
  );
}

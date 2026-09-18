import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { Ask } from "../../api/types";
import {
  calloutSuccessBodyText,
  linkHoverText,
  linkText,
  quoteAccentBorder,
  quoteBodyText,
  successQuoteAccentBorder,
  textPrimaryOnSuccessCallout,
} from "../../theme/classes";
import { buildIssuePath, buildProjectPath } from "../refs/routes";

/** The document named by an ask's quote anchor, including its stable block fragment when known. */
export function AskAnchorLink({ ask }: { ask: Ask }): ReactNode {
  const { anchor, anchor_artifact: artifact } = ask;
  if (anchor === null || artifact === undefined) {
    return null;
  }
  const fragment =
    anchor.block_id === undefined || anchor.block_id === null
      ? ""
      : `#b-${encodeURIComponent(anchor.block_id)}`;
  if (ask.issue_key !== null) {
    const route = artifact.primary
      ? buildIssuePath({ key: ask.issue_key, kind: "spec" })
      : buildIssuePath({ key: ask.issue_key, kind: "artifact", slug: artifact.slug });
    return (
      <Link className="underline" to={`${route}${fragment}`}>
        {artifact.name}
      </Link>
    );
  }
  return (
    <Link
      className="underline"
      to={`${buildProjectPath({ kind: "document", project: artifact.project, slug: artifact.slug })}${fragment}`}
    >
      {artifact.name}
    </Link>
  );
}

/** The document link and quoted passage that give an anchored ask its context. */
export function AskAnchorHeader({
  ask,
  inBlock,
  quoteSpacing = "compact",
  tone,
}: {
  ask: Ask;
  inBlock: boolean;
  quoteSpacing?: "compact" | "roomy";
  tone: "success" | "surface";
}): ReactNode {
  const { anchor, anchor_artifact: artifact } = ask;
  if (inBlock || anchor === null) {
    return null;
  }
  const success = tone === "success";
  const quoteClassName = success
    ? `${successQuoteAccentBorder} ${calloutSuccessBodyText}`
    : `${quoteAccentBorder} ${quoteBodyText}`;
  return (
    <>
      {artifact === undefined ? null : (
        <p
          className={
            success
              ? `mb-2 font-medium ${textPrimaryOnSuccessCallout}`
              : `mb-2 text-sm font-medium ${linkText} ${linkHoverText}`
          }
        >
          <AskAnchorLink ask={ask} />
        </p>
      )}
      <blockquote
        className={`${quoteSpacing === "roomy" ? "mb-3" : "mb-2"} border-l-2 pl-3 ${quoteClassName}`}
      >
        {anchor.quote}
      </blockquote>
    </>
  );
}

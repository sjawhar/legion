import { type ReactNode, useRef } from "react";

import { CopyButton } from "../../components/CopyButton";
import { useKeymap } from "../shell/keymap";

/** **Copy Markdown**, beside a document's link button: copies the version on screen as Markdown,
 *  which `read` reads when the button is pressed (`DocumentToolbar.readMarkdown`), and confirms as
 *  the link button does. While it is on the page the `$mod+k` palette offers the same copy, which
 *  clicks this button so the confirmation shows beside it. A failed copy says so without printing
 *  the document, which can run to a megabyte. */
export function CopyMarkdownButton({
  className,
  read,
}: {
  className?: string;
  read(): Promise<string>;
}): ReactNode {
  const root = useRef<HTMLSpanElement>(null);
  const button = () => root.current?.querySelector("button") ?? null;
  // Palette-only, in the scope the document's page pushes (`ProofDocument`).
  useKeymap("document", [
    {
      id: "copy-markdown",
      keys: [],
      label: "Copy Markdown",
      run: () => button()?.click(),
      when: () => button() !== null,
    },
  ]);
  return (
    <span className="contents" data-copy-markdown="" ref={root}>
      <CopyButton
        className={className}
        failure="try again"
        label="Copy Markdown"
        value={() => read()}
        what="Markdown"
      >
        Markdown
      </CopyButton>
    </span>
  );
}

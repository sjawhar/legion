import type { ReactNode } from "react";

/** The plain text of `node` when it is only strings and numbers, as `{key} · {title}` is. */
function textOf(node: ReactNode): string | undefined {
  if (typeof node === "string" || typeof node === "number") {
    return String(node);
  }
  if (Array.isArray(node)) {
    const parts = node.map(textOf);
    return parts.every((part) => part !== undefined) ? parts.join("") : undefined;
  }
  return undefined;
}

/**
 * Text that ends in an ellipsis when it does not fit inside a control that is a flex container:
 * any link below 1280 px, where `styles.css` makes every link an inline-flex box, and any button
 * or link that is `inline-flex` by its own classes. `text-overflow` never applies to a flex
 * container's own text, so `truncate` on the control alone cuts the text mid-word with no
 * ellipsis. This span is the flex item the ellipsis applies to; where the control is a block, the
 * span is inline and the control's own `truncate` draws it. The full text is its `title`, so a
 * pointer reader can see what was cut; `title` overrides that, and `null` sets none.
 */
export function TruncatedText({
  children,
  className = "",
  title,
}: {
  children: ReactNode;
  className?: string;
  title?: string | null;
}): ReactNode {
  return (
    <span
      className={`truncate ${className}`}
      title={title === undefined ? textOf(children) : (title ?? undefined)}
    >
      {children}
    </span>
  );
}

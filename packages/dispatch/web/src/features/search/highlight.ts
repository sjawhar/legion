/**
 * Replaces the previous search mark, then highlights the first matching rendered text node.
 * Text split across elements deliberately remains unmatched.
 */
export function highlightQuery(root: HTMLElement, query: string): boolean {
  for (const mark of root.querySelectorAll("mark[data-dispatch-search-hit]")) {
    mark.replaceWith(...mark.childNodes);
  }

  if (query === "") {
    return true;
  }

  const lowerQuery = query.toLowerCase();
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  let node = walker.nextNode();
  while (node !== null) {
    const text = node.nodeValue ?? "";
    const from = text.toLowerCase().indexOf(lowerQuery);
    if (from !== -1) {
      const range = document.createRange();
      range.setStart(node, from);
      range.setEnd(node, from + query.length);
      const mark = document.createElement("mark");
      mark.className = "dispatch-anchor-history";
      mark.dataset.dispatchSearchHit = "";
      range.surroundContents(mark);
      mark.scrollIntoView({ block: "center" });
      return true;
    }
    node = walker.nextNode();
  }

  return false;
}

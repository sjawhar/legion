/**
 * Draws a checkbox for every GFM task item (`- [x] done`, `- [ ] todo`) under `root`, ticked as
 * its source is. The schema writes a task item as `<li data-item-type="task" data-checked="…">`
 * with no box of its own, so without this pass a ticked and an unticked item look alike.
 *
 * It runs after the serializer has painted the element, not inside it: a node's serializer
 * function cannot reach into the item's child paragraph, and ProseMirror refuses a content hole
 * that sits beside a sibling element, so the item itself cannot carry the box. The box goes first
 * inside the item's first child (its paragraph, since a list item's content is `paragraph block*`):
 * a tight item's paragraph is then unwrapped with the box in it (`MarkdownBody`'s paint), and in a
 * loose or nested item the box sits inline on the first line of text.
 *
 * The box is `disabled` because that is the only native checkbox that is truly read-only: a
 * reader cannot tick it, it takes no tab stop, and it swallows no keyboard shortcut. It has no
 * label because the item's own words follow it; a label would have them spoken twice.
 */
export function drawTaskCheckboxes(root: HTMLElement): void {
  for (const item of root.querySelectorAll<HTMLLIElement>('li[data-item-type="task"]')) {
    const paragraph = item.firstElementChild;
    if (paragraph === null) {
      continue;
    }
    const box = document.createElement("input");
    box.type = "checkbox";
    box.disabled = true;
    box.setAttribute("data-markdown-task-checkbox", "");
    box.defaultChecked = item.getAttribute("data-checked") === "true";
    paragraph.prepend(box);
  }
}

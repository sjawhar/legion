/** A Testing Library text matcher for the element whose whole text is `text`: a line that names
 *  an agent draws it through `AgentName`, which splits the line's text across elements that a
 *  plain text match reads one at a time. The deepest such element matches, not its ancestors. */
export function wholeText(text: string) {
  return (_content: string, element: Element | null): boolean =>
    element?.textContent === text &&
    [...element.children].every((child) => child.textContent !== text);
}

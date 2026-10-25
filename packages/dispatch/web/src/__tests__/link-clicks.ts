/** Whether the browser would follow the last click on a link in `root`, read as the click leaves
 * the link. happy-dom follows one nothing prevented by moving the test's page, so each click is
 * prevented there, after the app's own handling. */
export function watchLinkClicks(root: HTMLElement): () => boolean | undefined {
  let followed: boolean | undefined;
  for (const link of root.querySelectorAll("a")) {
    link.addEventListener("click", (event) => {
      followed = !event.defaultPrevented;
      event.preventDefault();
    });
  }
  return () => {
    const last = followed;
    followed = undefined;
    return last;
  };
}

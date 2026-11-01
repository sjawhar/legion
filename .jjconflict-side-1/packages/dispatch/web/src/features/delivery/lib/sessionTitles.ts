// The delivery read's `session_titles` holds a session's id when the session has no title. Pure:
// no React.

/** A session's stored title from the delivery read's `session_titles`, or undefined when it has
 *  none (the map holds its id, or nothing). */
export function storedSessionTitle(
  titles: Readonly<Record<string, string>>,
  session: string
): string | undefined {
  const title = titles[session];
  return title === session ? undefined : title;
}

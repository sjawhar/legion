// The tmux server a session runs under, as the session's own `TMUX` names it. The title a pane
// registers and the place it reports both start from it, so the split lives here rather than
// twice over in the two readers.

/** The server `TMUX` names: the socket it answers on, and the process id of the server itself.
 *  Both are empty when `TMUX` names neither, which is every session outside tmux. */
export function tmuxServer(value: string | undefined): {
  readonly serverPid: string;
  readonly socket: string;
} {
  // `TMUX` is `<socket>,<server pid>,<session>`, and a socket path may hold a comma.
  const parts = (value ?? "").split(",");
  return { serverPid: parts.at(-2) ?? "", socket: parts.slice(0, -2).join(",") };
}

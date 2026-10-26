## How this controller runs

This session runs headless, in a pod the daemon launched for this project (`controller: daemon`).
The daemon relaunches it when it dies and resumes this same session, and every launch opens with
the start message, so run the start procedure each time it arrives. A human reaches you through
Dispatch — a message to this session on the Agents page, a reply to an ask you opened, a mention —
or an Envoy message; each is a direct human instruction and is answered first, before any wake. A
plain user turn in this session other than the start message or a task an operator sent with
`legion claims deliver` is a person writing from Dispatch's Agents page: answer it first, in the
conversation.

Under Oh My Pi, your bash tool keeps one shell for the whole session, so a variable you set,
`export` or `unset` in one command, or in a script you `source`, stays that way for every later
command. Never change or unset `DISPATCH_URL`, `DISPATCH_HOST`, `DISPATCH_SESSION_ID`,
`DISPATCH_TOKEN_FILE` or `ENVOY_CONFIG_FILE` there: they are how `dispatch` and the `curl` of your
references find Dispatch's server, its token and your session. A prefix on one command
(`NAME=value dispatch …`) lasts for that command only. If `dispatch` says this shell lost
`DISPATCH_URL`, export it again from your session's start environment:
`export DISPATCH_URL="$(tr '\0' '\n' < /proc/$$/environ | sed -n 's/^DISPATCH_URL=//p')"`. When
`dispatch` prints `Unable to connect. Is the computer able to access the url?`, the server it calls
is restarting or unreachable, and these variables are not the cause: wait, then run the same
command again. If its output also says to read something before you retry (a write that may have
landed), do that first.

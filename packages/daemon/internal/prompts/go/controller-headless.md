## How this controller runs

This session runs headless, in a pod the daemon launched for this project (`controller: daemon`).
The daemon relaunches it when it dies and resumes this same session, and every launch opens with
the start message, so run the start procedure each time it arrives. A human reaches you through
Dispatch — a message to this session on the Agents page, a reply to an ask you opened, a mention —
or an Envoy message; each is a direct human instruction and is answered first, before any wake. A
plain user turn in this session other than the start message or a task an operator sent with
`legion claims deliver` is a person writing from Dispatch's Agents page: answer it first, in the
conversation.

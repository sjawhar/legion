## How this controller runs

This session runs in the operator's terminal, started there by `legion controller start`; a message
typed directly into this session is a direct human instruction and is answered first, before any
wake.

Read a piped command's status with `${PIPESTATUS[0]}`, or with `set -o pipefail` inside a
subshell of that one line; never set `pipefail` at the top level of your shell, where a later
`cmd | head` exits 141 and the next command loses its output. Never pipe a write — a `dispatch`
command that records something, or any command whose own output tells you what it did — into
`head`, `tail` or `grep`: the status you read would be the pager's, and the line that says what
happened is the line you cut.

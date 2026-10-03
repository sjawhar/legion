"""The walkthrough's cut: which footage plays, in what order, and the narration laid over it.

Each clip is a window of one raw file, cut hard: no speed change, no held frame. Its start, its
end and each narration part's start are a mark the recorder set in that file
(walkthrough.record.ts's `mark(...)`, recorded in raw/sections.json) plus an offset in seconds, so
a new take re-times the cut itself. `start` and `end` are every file's first and last instants.
build.py fails when a part runs past its clip's end, into the next part, or starts before its
clip. The narration was written after the cut, to each clip's measured length, and says only what
the clip shows.

The video opens on its payoff, the command that received the key, then shows how it got there in
order: the machine's login, its approval, the session, the request, its approval, the command
running, and the live grant.
"""

from __future__ import annotations

from typing import NamedTuple


class At(NamedTuple):
    """A moment in a clip's source: the time of `mark` plus `offset` seconds."""

    mark: str
    offset: float = 0.0


class Clip(NamedTuple):
    id: str
    source: str
    start: At
    end: At
    narration: tuple[tuple[str, At], ...] = ()


# The narration parts in speaking order; narrate.py sends each with its neighbours' text. Names are
# written as they are said: DEMO_API_KEY as "the demo API key".
NARRATION: dict[str, str] = {
    "open": "This command just got a key that a person approved.",
    "login": "First, the machine logs in, and prints a code to enter in Dispatch.",
    "machine-code": "There, alice types it into the machine login page.",
    "machine-approve": "It names example-host-build. She approves it.",
    "session-issued": "Back on the machine, the login is issued.",
    "session-register": "Next, it registers a session for an agent.",
    "session-self": "Its operator is alice.",
    "request-ask": "Now the session asks for the demo API key to run one command, and says why.",
    "request-wait": "Now it waits for a person.",
    "approve-inbox": "The request is in alice's Inbox.",
    "approve-record": "Its page shows the session, the lifetime, and the agent's reason.",
    "approve-click": "She approves it.",
    "ran-runs": "The waiting command gets the key, and runs.",
    "ran-status": "Its status says alice decided it.",
    "grants-list": "Settings lists the live grant, with alice as its approver.",
    "grants-revoke": "Revoke ends its access at once.",
}

CLIPS: list[Clip] = [
    Clip("open", "t4-ran.cast", At("start", 0.15), At("status-typing", -0.2), (("open", At("start", 0.35)),)),
    Clip("login", "t1-login.cast", At("typing", -0.3), At("code", 2.5), (("login", At("typing", -0.1)),)),
    Clip("machine", "b1-machine.webm", At("page", 0.3), At("result", 1.4),
         (("machine-code", At("page", 0.6)), ("machine-approve", At("record", 0.4)))),
    Clip("session", "t2-session.cast", At("status-typing", -0.3), At("self", 1.5),
         (("session-issued", At("status-typing", 0.0)), ("session-register", At("register-typing", 0.0)),
          ("session-self", At("self", -0.6)))),
    Clip("request", "t3-request.cast", At("typing", -0.3), At("waiting", 2.4),
         (("request-ask", At("typing", 0.1)), ("request-wait", At("waiting", -0.4)))),
    Clip("approve", "b2-approve.webm", At("inbox", -0.3), At("result", 1.0),
         (("approve-inbox", At("inbox", -0.1)), ("approve-record", At("record", -0.4)), ("approve-click", At("approve", -1.0)))),
    Clip("ran", "t4-ran.cast", At("start", 2.5), At("decided", 2.5),
         (("ran-runs", At("start", 2.7)), ("ran-status", At("decided", -0.2)))),
    Clip("grants", "b3-grants.webm", At("settings", -0.3), At("result", 1.0),
         (("grants-list", At("grants", 0.4)), ("grants-revoke", At("revoke", 0.5)))),
]

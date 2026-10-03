"""The walkthrough's cut: which footage plays, in what order, and the narration laid over it.

Each clip is a window of one raw file in that file's own seconds (a cast's event times, a browser
recording's timestamps), cut hard: no speed change, no held frame. Its narration parts start at
offsets from the clip's start; build.py fails when one runs past its clip's end or into the next
part. The narration was written after the cut, to each clip's measured length, and says only what
the clip shows.

The video opens on its payoff, the command that received the key, then shows how it got there in
order: the machine's login, its approval, the session, the request, its approval, the command
running, and the live grant.
"""

from __future__ import annotations

from typing import NamedTuple


class Clip(NamedTuple):
    id: str
    source: str
    start: float
    end: float
    narration: tuple[tuple[str, float], ...] = ()

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
    Clip("open", "t4-ran.cast", 0.15, 5.85, (("open", 0.2),)),
    Clip("login", "t1-login.cast", 0.25, 6.74, (("login", 0.2),)),
    Clip("machine", "b1-machine.webm", 1.8, 14.6, (("machine-code", 0.3), ("machine-approve", 6.2))),
    Clip("session", "t2-session.cast", 0.8, 15.2,
         (("session-issued", 0.3), ("session-register", 4.6), ("session-self", 11.2))),
    Clip("request", "t3-request.cast", 0.8, 12.6, (("request-ask", 0.4), ("request-wait", 9.0))),
    Clip("approve", "b2-approve.webm", 1.2, 13.6,
         (("approve-inbox", 0.2), ("approve-record", 3.9), ("approve-click", 9.75))),
    Clip("ran", "t4-ran.cast", 2.5, 13.8, (("ran-runs", 0.2), ("ran-status", 8.0))),
    Clip("grants", "b3-grants.webm", 1.5, 12.5, (("grants-list", 0.4), ("grants-revoke", 7.0))),
]

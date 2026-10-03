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

import json
from pathlib import Path
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


# The narration parts in speaking order, from narration.json, which narrate.ts reads too; narrate.ts
# sends each with its neighbours' text. Names are written as they are said: DEMO_API_KEY as "the
# demo API key".
NARRATION: dict[str, str] = json.loads((Path(__file__).parent / "narration.json").read_text())

# The ran beat is three windows of one cast. ran ends and ran-status starts on the same frozen frame
# (the command's line under a fresh prompt), so that join shows no seam while it drops the seconds
# before anyone types; ran-status ends at `status-word`, once `agent-secrets status ` is typed and
# before the request id's first character, and ran-result jumps the id, typed a character at a
# time, to its end. The recorder sets `status-word` just before it sends the id's first character,
# so the cut sits a few hundredths of a second before the mark.
#
# A cast's `end` is the detached client's farewell, which clears the screen to `[detached (from
# session agent)]`; agg's frames can show it a frame or two before its time, so a clip that plays
# to the end of a cast stops 0.2 s before `end`.
CLIPS: list[Clip] = [
    # The payoff: the command's line, just after the key reached it.
    Clip("open", "t4-ran.cast", At("key", 0.05), At("key", 4.35), (("open", At("key", 0.1)),)),
    Clip("login", "t1-login.cast", At("start", 0.2), At("end", -0.2), (("login", At("start", 0.35)),)),
    Clip("machine", "b1-machine.webm", At("page", 0.2), At("result", 1.4),
         (("machine-code", At("typing", -1.2)), ("machine-approve", At("record", 0.6)))),
    Clip("session", "t2-session.cast", At("status-typing", -0.3), At("self", 4.4),
         (("session-issued", At("status-typing", -0.2)), ("session-register", At("register-typing", -0.1)),
          ("session-self", At("self-typing", -0.3)))),
    Clip("request", "t3-request.cast", At("typing", -0.4), At("end", -0.2),
         (("request-ask", At("typing", -0.2)), ("request-wait", At("waiting", 0.0)))),
    # Opens on the request in the Inbox: before `inbox` the page may still be loading.
    Clip("approve", "b2-approve.webm", At("inbox"), At("result", 1.3),
         (("approve-inbox", At("inbox", 0.0)), ("approve-record", At("record", -0.2)), ("approve-click", At("approve", -0.6)))),
    # The waiting command, still waiting just after the approval, gets the key and runs.
    Clip("ran", "t4-ran.cast", At("key", -1.5), At("key", 2.7), (("ran-runs", At("key", -1.3)),)),
    Clip("ran-status", "t4-ran.cast", At("status-typing", -0.2), At("status-word", -0.04)),
    Clip("ran-result", "t4-ran.cast", At("status-typed", -0.3), At("decided", 3.2),
         (("ran-status", At("decided", -0.1)),)),
    # From the Settings click: the live grant's row, its approver, its Revoke button.
    Clip("grants", "b3-grants.webm", At("settings", -0.8), At("result", 1.4),
         (("grants-list", At("row", 0.0)), ("grants-revoke", At("revoke", 1.7)))),
]

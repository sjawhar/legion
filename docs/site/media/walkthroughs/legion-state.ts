// The operator's terminal on a running Legion daemon: `legion status`, `legion state`, an issue's
// record, and `legion claims list`. One cast, `legion-issue-journey/state.cast`, which
// `legion-issue-journey/record-casts.sh` records; the narration was written to its measured clip.
import type { Walkthrough } from "../recording";

const legionState: Walkthrough<never> = {
  title: "Read a Legion daemon's state",
  sections: [
    {
      id: "state",
      cast: "legion-issue-journey/state.cast",
      // The whole session but the detach.
      window: [1, 21.2],
      narration: [
        { at: 1.6, text: "legion status says the daemon is running." },
        { at: 5.2, text: "legion state prints its admission slots and the issues it records." },
        { at: 11.2, text: "Its JSON shows this issue's architect, ready." },
        { at: 16.9, text: "legion claims list shows each claim." },
      ],
    },
  ],
};

export default legionState;

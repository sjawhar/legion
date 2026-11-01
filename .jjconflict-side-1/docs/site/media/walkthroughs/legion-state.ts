// The operator's terminal on an example Legion daemon: `legion status`, `legion state`, an issue's
// record, and `legion claims list`. One cast, `legion-operator/state.cast`, which
// `legion-operator/record-casts.sh` records from the same daemon `legion-controller`'s cast
// registers with; the narration was written to its measured clip.
import type { Walkthrough } from "../recording";

const legionState: Walkthrough<never> = {
  title: "Read a Legion daemon's state",
  poster: "last",
  sections: [
    {
      id: "state",
      cast: "legion-operator/state.cast",
      // The whole session but the detach.
      window: [1, 22.3],
      narration: [
        { at: 1.6, text: "legion status says the daemon is running." },
        { at: 5.3, text: "legion state prints its admission slots and the issues it records." },
        { at: 12, text: "Its JSON shows this issue's architect, ready." },
        { at: 18.2, text: "legion claims list shows each claim." },
      ],
    },
  ],
};

export default legionState;

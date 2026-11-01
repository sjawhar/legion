// `legion controller start` bringing up a project's controller, and the daemon recording it. One
// cast, `legion-operator/controller.cast`, which `legion-operator/record-casts.sh` records against
// the same daemon `legion-state`'s cast reads; the narration was written to its measured clip.
import type { Walkthrough } from "../recording";

const legionController: Walkthrough<never> = {
  title: "Start the controller",
  poster: "last",
  sections: [
    {
      id: "controller",
      cast: "legion-operator/controller.cast",
      // The whole session but the detach.
      window: [0.9, 17],
      narration: [
        { at: 2.9, text: "It checks the controller's Oh My Pi," },
        { at: 5.4, text: "then starts it against the daemon." },
        { at: 7.7, text: "The controller opens on its start procedure," },
        { at: 12.7, text: "and the daemon records it as the project's controller." },
      ],
    },
  ],
};

export default legionController;

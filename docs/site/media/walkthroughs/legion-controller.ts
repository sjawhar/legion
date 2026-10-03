// `legion controller start` bringing up a project's controller, and the daemon recording it. One
// cast, `legion-issue-journey/controller.cast`, which `legion-issue-journey/record-casts.sh`
// records; the narration was written to its measured clip.
import type { Walkthrough } from "../recording";

const legionController: Walkthrough<never> = {
  title: "Start the controller",
  sections: [
    {
      id: "controller",
      cast: "legion-issue-journey/controller.cast",
      // The whole session but the detach.
      window: [0.9, 17],
      narration: [
        { at: 3.2, text: "It checks the controller's Oh My Pi," },
        { at: 5.75, text: "then starts it against the daemon." },
        { at: 7.6, text: "The controller opens on its start procedure," },
        { at: 12.7, text: "and the daemon records it as the project's controller." },
      ],
    },
  ],
};

export default legionController;

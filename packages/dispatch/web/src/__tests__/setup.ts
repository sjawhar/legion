import { GlobalRegistrator } from "@happy-dom/global-registrator";
import { notifyManager } from "@tanstack/react-query";

GlobalRegistrator.register();

// TanStack batches observer notifications through a scheduler that happy-dom does not
// drive promptly; flushing synchronously keeps query updates inside React's act() scope
// so assertions after setQueryData do not wait on a stalled timer.
notifyManager.setScheduler((callback) => callback());

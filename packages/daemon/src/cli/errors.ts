import { WORKSPACE_LOST_EXIT_CODE } from "../daemon/runtime";

export class CliError extends Error {
  constructor(
    message: string,
    readonly code = 1
  ) {
    super(message);
    this.name = "CliError";
  }
}

export class WorkspaceLostError extends CliError {
  constructor(message: string) {
    super(message, WORKSPACE_LOST_EXIT_CODE);
    this.name = "WorkspaceLostError";
  }
}

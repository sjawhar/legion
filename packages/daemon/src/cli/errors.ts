export class CliError extends Error {
  constructor(
    message: string,
    readonly code = 1
  ) {
    super(message);
    this.name = "CliError";
  }
}

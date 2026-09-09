export class IssueStateWriteQueue {
  private readonly tails = new Map<string, Promise<void>>();

  enqueue<T>(issueKey: string, operation: () => Promise<T>): Promise<T> {
    const previous = this.tails.get(issueKey) ?? Promise.resolve();
    const result = previous.then(operation, operation);
    const settled = result.then(
      () => undefined,
      () => undefined
    );
    this.tails.set(issueKey, settled);
    void settled.then(() => {
      if (this.tails.get(issueKey) === settled) {
        this.tails.delete(issueKey);
      }
    });
    return result;
  }
}

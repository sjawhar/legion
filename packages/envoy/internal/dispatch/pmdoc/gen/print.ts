// print.ts - how a gen script hands its result to the Go test that reads its stdout.
//
// The headless editor's import graph imports node:process as a module (vfile, through unified),
// and Bun opens fd 1 non-blocking when that module loads. On a non-blocking pipe console.log
// writes what the pipe has room for, drops the rest and the script still exits 0, so a test that
// read the pipe late, as a loaded machine's does, got a line cut short. Bun.write(Bun.stdout, ...)
// is no better there: at Bun 1.3.14, after the pipe refuses it, it writes the whole text again
// from its start and then never finishes. process.stdout.write keeps what the pipe cannot take
// and writes it as the reader drains the pipe; its callback runs once the last byte is written, or
// with the error that stopped it, which the await throws, so the script exits non-zero rather than
// printing less than it says (gen_output_linux_test.go holds a stalled reader to that).

// printLine writes text and a line feed to stdout and resolves once the pipe has taken all of it.
export async function printLine(text: string): Promise<void> {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  process.stdout.write(`${text}\n`, (error) => (error ? reject(error) : resolve()));
  await promise;
}

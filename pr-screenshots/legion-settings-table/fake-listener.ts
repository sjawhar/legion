// Throwaway fake Envoy listener for the settings smoke: answers GET /v1/sessions with no sessions
// and appends the Authorization header of every request to the file named by its second argument.
import { appendFileSync } from "node:fs";

const [port, log] = process.argv.slice(2);
Bun.serve({
  hostname: "127.0.0.1",
  port: Number(port),
  fetch(request) {
    appendFileSync(
      log,
      `${request.method} ${new URL(request.url).pathname} authorization=${request.headers.get("authorization") ?? "(none)"}\n`
    );
    return Response.json([]);
  },
});
console.log(`fake listener on ${port}`);

// A stand-in for whatever answers in front of Dispatch (a load balancer, a proxy, an auth gateway)
// for driving a client against an answer Dispatch itself never gives. Every request gets the one
// configured answer (`src/stand-in-gateway.ts`), and each is logged on stderr as
// `METHOD /path?query`, which shows what the client sent (never its bearer, which travels in a
// header).
//   bun bin/stand-in-gateway.ts [--status 502] [--body html|empty|text|json] [--port 0]
// Prints `listening http://127.0.0.1:<port>` once it serves, then runs until it is killed.
import { messageFor } from "../src/errors";
import { parseStandInArguments, type StandInOptions, standInAnswer } from "../src/stand-in-gateway";

let options: StandInOptions;
try {
  options = parseStandInArguments(Bun.argv.slice(2));
} catch (error) {
  console.error(`stand-in-gateway: ${messageFor(error)}`);
  process.exit(2);
}
const server = Bun.serve({
  port: options.port,
  hostname: "127.0.0.1",
  fetch(request) {
    const url = new URL(request.url);
    console.error(`${request.method} ${url.pathname}${url.search}`);
    return standInAnswer(options.status, options.body);
  },
});
console.log(`listening http://127.0.0.1:${server.port}`);

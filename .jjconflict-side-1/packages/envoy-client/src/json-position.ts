/** Where `text` stops being JSON, as `line L, column C`, or undefined when it is JSON. Worked out
 * without the parser's message, which quotes the offending token: an unquoted token or seed in an
 * envoy.json would land in the error. */
export function jsonErrorPosition(text: string): string | undefined {
  let at = 0;
  const fail = (): never => {
    throw new RangeError("not JSON");
  };
  const space = (): void => {
    while (at < text.length && " \t\n\r".includes(text.charAt(at))) at += 1;
  };
  const expect = (char: string): void => {
    if (text.charAt(at) !== char) fail();
    at += 1;
  };
  const string = (): void => {
    expect('"');
    while (at < text.length) {
      const char = text.charAt(at);
      if (char === '"') {
        at += 1;
        return;
      }
      if (char < " ") fail();
      at += char === "\\" ? 2 : 1;
    }
    fail();
  };
  const value = (): void => {
    space();
    const char = text.charAt(at);
    if (char === "{" || char === "[") {
      const close = char === "{" ? "}" : "]";
      at += 1;
      space();
      if (text.charAt(at) === close) {
        at += 1;
        return;
      }
      for (;;) {
        if (close === "}") {
          space();
          string();
          space();
          expect(":");
        }
        value();
        space();
        if (text.charAt(at) === ",") {
          at += 1;
          continue;
        }
        expect(close);
        return;
      }
    }
    if (char === '"') {
      string();
      return;
    }
    for (const word of ["true", "false", "null"]) {
      if (text.startsWith(word, at)) {
        at += word.length;
        return;
      }
    }
    const number = /^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/.exec(text.slice(at));
    if (number === null) fail();
    at += number?.[0].length ?? 0;
  };
  try {
    value();
    space();
    if (at < text.length) fail();
    return undefined;
  } catch {
    const before = text.slice(0, at).split("\n");
    return `line ${before.length}, column ${(before.at(-1)?.length ?? 0) + 1}`;
  }
}

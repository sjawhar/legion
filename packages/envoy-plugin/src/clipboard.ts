/**
 * Copy text to the user's clipboard via the OSC 52 escape sequence.
 *
 * Works across local terminals and SSH sessions when the terminal
 * emulator supports OSC 52 (iTerm2, Kitty, Alacritty, modern xterm,
 * tmux with `set -g set-clipboard on`, etc).
 *
 * Returns true if the sequence was written, false on error.
 */
export function copyOsc52(text: string): boolean {
  try {
    const b64 = Buffer.from(text, "utf-8").toString("base64");
    process.stdout.write(`\x1b]52;c;${b64}\x07`);
    return true;
  } catch {
    return false;
  }
}

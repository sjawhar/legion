const nestPrefix = (d) => Array.from({ length: d }, (_, i) => "  ".repeat(i) + "- a\n").join("");
const fill = (prefix, unit, cap = 32000) => prefix + unit.repeat(Math.floor((cap - prefix.length) / unit.length));
export const gen = {
  list_plain: (n) => "- x\n".repeat(n),
  list_words: (n) => "- some words here\n".repeat(n),
  para_lines: (n) => "x\n".repeat(n),
  quote_plain: (n) => "> x\n".repeat(n),
  quote_lazy: (n) => "> x\n" + "x\n".repeat(n),
  item_lazy: (n) => "- x\n" + "x\n".repeat(n),
  brackets: (n) => "[".repeat(n) + "]".repeat(n),
  brackets_callout: (n) => ":::callout[" + "[".repeat(n) + "]".repeat(n),
  nest100_x: (n) => nestPrefix(100) + "x\n".repeat(n),
  nest100_blanks: (n) => nestPrefix(100) + "\n".repeat(n),
  nest100_blank_x: (n) => nestPrefix(100) + "\nx\n".repeat(n),
  nestD_blanks: (n) => nestPrefix(n) + "\n".repeat(4000),
  nestD_x: (n) => nestPrefix(n) + "x\n".repeat(4000),
  dash_chain: (n) => "- ".repeat(n) + "x\n",
  quote_chain: (n) => "> ".repeat(n) + "x\n",
  fill,
};

// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
import type { MilkdownPlugin } from '@milkdown/ctx';
import { $remark } from '@milkdown/kit/utils';
import remarkFrontmatter from 'remark-frontmatter';

export const libraryRemarkFrontmatterPlugin: MilkdownPlugin = $remark(
  'remarkFrontmatter',
  () => remarkFrontmatter,
  ['yaml'],
);

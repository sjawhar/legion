import { $remark } from '@milkdown/kit/utils';
import remarkFrontmatter from 'remark-frontmatter';

export const libraryRemarkFrontmatterPlugin = $remark(
  'remarkFrontmatter',
  () => remarkFrontmatter,
  ['yaml'],
);

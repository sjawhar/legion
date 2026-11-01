export function canonicalRepo(owner: string, repo: string): string {
  return `${owner.trim().toLowerCase()}/${repo
    .trim()
    .toLowerCase()
    .replace(/\.git$/, "")}`;
}

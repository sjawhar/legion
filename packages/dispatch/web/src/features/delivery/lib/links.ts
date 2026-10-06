// GitHub link builders for the delivery timeline's drill-down. There is no local `links.ts`
// precedent in this codebase for building GitHub URLs (unlike a Dispatch issue link, which the
// SPA already builds through `features/refs/routes.ts`'s `buildIssuePath`), so these are built
// directly per LEGION-567's plan: "GitHub links built directly (`https://github.com/<repo>/pull/<n>`,
// `.../actions/runs/<id>`)".

export function githubPrUrl(repo: string, number: number): string {
  return `https://github.com/${repo}/pull/${number}`;
}

export function githubRunUrl(repo: string, runId: number): string {
  return `https://github.com/${repo}/actions/runs/${runId}`;
}

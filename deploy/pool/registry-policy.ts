export interface Revision {
  repository: string;
  digest: string;
  tags: string[];
  createdMs: number;
  usedMs: number;
  children: string[];
}

// Select roots first, then preserve every manifest reachable from any retained
// root. A child of an OCI index is not garbage merely because it has no tag.
export function registryVictims(revisions: Revision[], now: number): Revision[] {
  const day = 86400_000;
  const proposed = new Set<Revision>();
  const repositories = new Set(revisions.map(r => r.repository));
  for (const repository of repositories) {
    const group = revisions.filter(r => r.repository === repository).sort((a,b) => b.createdMs-a.createdMs || a.digest.localeCompare(b.digest));
    const recent = new Set(group.filter(r => r.tags.length > 0).slice(0,3));
    for (const r of group) {
      if (r.tags.some(tag => tag.startsWith('keep-'))) continue;
      if (now-r.createdMs < day) continue;
      if (now-r.usedMs >= 7*day || !recent.has(r) && now-r.usedMs >= day) proposed.add(r);
    }
  }
  const retained = new Set<string>();
  const children = new Map<string,string[]>();
  for (const r of revisions) children.set(r.digest,[...(children.get(r.digest) ?? []),...r.children]);
  const pending = revisions.filter(r => !proposed.has(r)).map(r => r.digest);
  while(pending.length){const digest=pending.pop()!;if(retained.has(digest))continue;retained.add(digest);pending.push(...children.get(digest) ?? []);}
  return revisions.filter(r => proposed.has(r) && !retained.has(r.digest));
}

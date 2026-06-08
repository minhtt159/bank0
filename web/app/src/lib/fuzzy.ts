// Tiny subsequence fuzzy matcher (docs/08 §4). Lists here are small (own
// accounts + saved payees), so a hand-rolled scorer beats pulling in a dep.

export function fuzzyScore(query: string, text: string): number {
  const q = query.toLowerCase();
  const t = text.toLowerCase();
  if (!q) return 1;
  let qi = 0;
  let score = 0;
  let streak = 0;
  for (let ti = 0; ti < t.length && qi < q.length; ti++) {
    if (t[ti] === q[qi]) {
      qi++;
      streak++;
      score += streak; // reward consecutive matches
    } else {
      streak = 0;
    }
  }
  return qi === q.length ? score : 0;
}

export function fuzzyFilter<T>(items: T[], query: string, key: (t: T) => string): T[] {
  if (!query.trim()) return items;
  return items
    .map((it) => ({ it, s: fuzzyScore(query, key(it)) }))
    .filter((x) => x.s > 0)
    .sort((a, b) => b.s - a.s)
    .map((x) => x.it);
}

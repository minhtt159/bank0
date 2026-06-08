// Money is int64 minor units end-to-end; format for display only, never compute
// on floats (docs/08 §8).

export function formatMinor(minor: number, currency = "EUR"): string {
  return new Intl.NumberFormat(undefined, { style: "currency", currency }).format(minor / 100);
}

// Parse a major-unit input ("12.50") to minor units, or null if invalid/≤0.
export function parseMajor(input: string): number | null {
  const n = Number(input.trim().replace(",", "."));
  if (!Number.isFinite(n) || n <= 0) return null;
  return Math.round(n * 100);
}

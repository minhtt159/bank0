// Money is int64 minor units end-to-end; format for display only, never compute
// on floats (docs/07 §8).

export function formatMinor(minor: number, currency = "EUR"): string {
  return new Intl.NumberFormat(undefined, { style: "currency", currency }).format(minor / 100);
}

// Parse a major-unit input ("12.50") to minor units, or null if invalid/≤0.
// Requires a plain decimal with at most 2 fraction digits, so sub-cent input
// ("0.001") is rejected rather than silently rounding to a 0-minor "valid"
// amount that would pass the transfer gate.
export function parseMajor(input: string): number | null {
  const s = input.trim().replace(",", ".");
  if (!/^\d+(\.\d{1,2})?$/.test(s)) return null;
  const minor = Math.round(Number(s) * 100);
  return minor > 0 ? minor : null;
}

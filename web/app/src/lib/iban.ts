// IBAN validation (ISO 13616 / ISO 7064 MOD-97-10). Mirrors internal/iban (Go) and
// the Postgres iban_is_valid() function. Client-side validation is for INSTANT UX
// only — the Go API and the Postgres CHECK are the authorities. The algorithm + the
// per-country length table were research-verified (see docs/11-iban-verification.md).

// Total IBAN length per ISO 3166-1 alpha-2 country code (SWIFT IBAN Registry).
export const COUNTRY_LENGTHS: Record<string, number> = {
  AL: 28, AD: 24, AT: 20, AZ: 28, BH: 22, BY: 28, BE: 16, BA: 20, BR: 29, BG: 22,
  BI: 27, CR: 22, HR: 21, CY: 28, CZ: 24, DK: 18, DJ: 27, DO: 28, TL: 23, EG: 29,
  SV: 28, EE: 20, FK: 18, FO: 18, FI: 18, FR: 27, GE: 22, DE: 22, GI: 23, GR: 27,
  GL: 18, GT: 28, HN: 28, HU: 28, IS: 26, IQ: 23, IE: 22, IL: 23, IT: 27, JO: 30,
  KZ: 20, XK: 20, KW: 30, LV: 21, LB: 28, LY: 25, LI: 21, LT: 20, LU: 20, MT: 31,
  MR: 27, MU: 30, MC: 27, MD: 24, MN: 20, ME: 22, NL: 18, NI: 28, MK: 19, NO: 15,
  OM: 23, PK: 24, PS: 29, PL: 28, PT: 25, QA: 29, RO: 24, RU: 33, LC: 32, SM: 27,
  ST: 25, SA: 24, RS: 22, SC: 31, SK: 24, SI: 19, SO: 23, ES: 24, SD: 18, SE: 24,
  CH: 21, TN: 24, TR: 26, UA: 29, AE: 23, GB: 22, VA: 22, VG: 24, YE: 30,
};

/** Strip whitespace + uppercase: printed form -> electronic form. */
export function normalizeIBAN(s: string): string {
  return s.replace(/\s+/g, "").toUpperCase();
}

/** Group in blocks of four for display. */
export function formatIBAN(s: string): string {
  return normalizeIBAN(s).replace(/(.{4})/g, "$1 ").trim();
}

/** Structure + exact per-country length + MOD-97 checksum. */
export function isValidIBAN(raw: string): boolean {
  const s = normalizeIBAN(raw);
  if (!/^[A-Z]{2}[0-9]{2}[A-Z0-9]+$/.test(s)) return false;
  const want = COUNTRY_LENGTHS[s.slice(0, 2)];
  if (!want || s.length !== want) return false;
  return mod97(s.slice(4) + s.slice(0, 4)) === 1;
}

// Running per-character fold (letters A=10..Z=35), accumulator stays < 9700 — no bignum.
function mod97(s: string): number {
  let rem = 0;
  for (let i = 0; i < s.length; i++) {
    const code = s.charCodeAt(i);
    if (code >= 48 && code <= 57) rem = (rem * 10 + (code - 48)) % 97; // '0'..'9'
    else rem = (rem * 100 + (code - 55)) % 97; // 'A'..'Z' -> 10..35
  }
  return rem;
}

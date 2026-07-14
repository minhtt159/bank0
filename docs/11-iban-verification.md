# IBAN verification & generation

> How the **bank0** core-banking backend validates and generates International Bank Account Numbers (IBANs), and where that logic sits across bank0's three surfaces. The MOD-97 algorithm and the per-country length table are both verified — an executable check of the algorithm and a four-source cross-check of the table both passed (see §2, §4).

---

## 1. Structure & standard (ISO 13616)

An IBAN (ISO 13616) is a single string of **up to 34 characters**, drawn only from `A–Z` and `0–9` (no lowercase semantics, no punctuation). It is composed left-to-right as:

| Part | Length | Content |
|---|---|---|
| Country code | 2 | ISO 3166-1 alpha-2 (e.g. `GB`, `DE`) |
| Check digits | 2 | ISO/IEC 7064 MOD-97-10 digits (positions 3–4) |
| BBAN | up to 30 | Basic Bank Account Number, country-specific layout |

Total = `2 + 2 + BBAN ≤ 34`. The **length is fixed per country** (e.g. 22 for GB/DE, 27 for FR, 31 for MT, 15 for NO), so an exact per-country length lookup is part of strict validation (§4). The BBAN's internal layout (bank code / branch / account number, and any *national* check digits) is country-specific, but the MOD-97 algorithm treats the BBAN as an opaque alphanumeric blob. ([ISO 13616 / Wikipedia](https://en.wikipedia.org/wiki/International_Bank_Account_Number); [iban.com/structure](https://www.iban.com/structure))

**Two representations.** The *electronic* form carries **no spaces** (one continuous string); the *printed* form groups it in blocks of four with single spaces, the last group being variable length — e.g. `GB82 WEST 1234 5698 7654 32`. Therefore step one of any validator is **strip all whitespace and uppercase** so both forms validate identically. ([Wikipedia](https://en.wikipedia.org/wiki/International_Bank_Account_Number))

> **Note on national check digits.** ~30 registry countries embed an *additional* national check digit inside the BBAN (e.g. ES "DC", FR/MC "RIB key", IT/SM "CIN", NO mod-11), and some bake in a fixed constant (BA always `39`, TL `38`, MK `07`, ME `25`, PT `50`, RS `35`, SI `56`, TN `59`, MR `13`). **Do not re-validate these** — their algorithms differ per country. Always validate the two ISO-13616 check digits (positions 3–4) for every IBAN; that is the cross-country invariant. ([Wikipedia ISO 13616 table](https://en.wikipedia.org/wiki/International_Bank_Account_Number))

---

## 2. Validation algorithm (MOD-97-10 / ISO 7064)

The two check digits are computed under **ISO/IEC 7064, MOD 97-10**. A correctly formed IBAN always satisfies `number mod 97 == 1`. 97 is used because it is the largest prime below 100, maximizing detection of single-digit errors and most transpositions. ([Wikipedia](https://en.wikipedia.org/wiki/International_Bank_Account_Number); [iban.com](https://www.iban.com/structure))

**Step order:** strip/upcase → character + per-country length check → rotate first 4 chars to the end → map letters to two digits (`A=10 … Z=35`; digits unchanged; ASCII shortcut `value = charCode - 55`) → interpret as one base-10 integer → valid iff `integer mod 97 == 1`. ([Wikipedia](https://en.wikipedia.org/wiki/International_Bank_Account_Number); [iban.com](https://www.iban.com/structure))

The expanded integer can exceed 40 digits, overflowing 64-bit ints, so **mod 97 is computed piecewise left-to-right, carrying the running remainder** — no bignum required.

### Verified pseudocode

```
# Piecewise mod-97. Two carried digits (remainder 0..96) + 7 new digits = <=9
# digits per chunk, staying below 10^9 < 2^31. (A per-character fold,
# rem = (rem*10 + d) % 97, is equally correct and was the form executed below.)
function mod97(D):                       # D = all-digit string
    remainder = 0
    i = 0
    while i < length(D):
        chunk     = toString(remainder) + substring(D, i, i+7)
        remainder = parseInt(chunk, base=10) mod 97
        i         = i + 7
    return remainder

function isValidIBAN(raw):
    s = uppercase(removeAllSpaces(raw))
    if not matches(s, /^[A-Z0-9]+$/):        return false
    if length(s) < 15 or length(s) > 34:     return false
    cc = substring(s, 0, 2)
    if cc not in COUNTRY_LENGTHS:            return false
    if length(s) != COUNTRY_LENGTHS[cc]:     return false   # exact per-country length (§4)
    rotated = substring(s, 4) + substring(s, 0, 4)          # move first 4 chars to end
    digits = ""
    for ch in rotated:
        if ch in '0'..'9': digits += ch
        else:              digits += toString(charCode(ch) - 55)   # 'A'(65)->10 .. 'Z'(90)->35
    return mod97(digits) == 1
```

> A robust validator should assert structural form *before* the letter mapping: char 0–1 alpha, char 2–3 digit, all alphanumeric — otherwise a stray non-alnum char could slip into the digit mapping. ([Wikipedia](https://en.wikipedia.org/wiki/International_Bank_Account_Number); [fakemyinfo MOD-97 walkthrough](https://fakemyinfo.com/guides/iban-validation-mod97-explained.html))

**Canonical worked example** — `GB82 WEST 1234 5698 7654 32`: rotate `GB82` to the end → `WEST12345698765432GB82`; expand letters (`W=32,E=14,S=28,T=29,G=16,B=11`) → `3214282912345698765432161182`; that integer `mod 97 == 1`, so the check passes. Use this as the canonical implementation fixture. ([Wikipedia](https://en.wikipedia.org/wiki/International_Bank_Account_Number))

> **Verified.** A Python implementation of the validator + piecewise mod-97 was executed adversarially (exit 0, ALL PASS): 9 known-valid IBANs (`GB82WEST12345698765432`, `DE89370400440532013000`, `FR1420041010050500013M02606`, `NL91ABNA0417164300`, `SE4550000000058398257466`, `AE070331234567890123456`, `SA0380000000608010167519`, `NO9386011117947`, `CH9300762011623852957`) all returned valid; corrupted strings (last digit flipped, `XX0000`) all returned invalid.

---

## 3. Generation of valid (synthetic) IBANs

Generation is **validation run in reverse**. Given a country code and a BBAN of that country's fixed length/character profile:

1. Form `rearranged = BBAN + countryCode + "00"` (BBAN first, then country, then **two zero placeholders** — getting this order wrong silently yields wrong digits).
2. Map letters `A=10 … Z=35`, digits unchanged → numeric string.
3. `remainder = mod97(numeric)` (same piecewise routine as §2).
4. `checkDigits = 98 - remainder`, **left-padded to 2 digits** (e.g. `9 → "09"`).
5. `IBAN = countryCode + checkDigits + BBAN`.

`98 - (N mod 97)` is the unique value congruent to `(1 - N) mod 97`, so prepending it makes the final rearranged integer `≡ 1 (mod 97)`. For the `00`-placeholder construction the result is always in `02..98`, never `00`/`01`. ([Wikipedia](https://en.wikipedia.org/wiki/International_Bank_Account_Number); [Mod-97 generation write-up](https://medium.com/@matlabb/iban-structure-and-mod-97-validation-algorithm-719e3d4db5f2))

```
function generateIBAN(countryCode, bban):
    rearranged = bban + countryCode + "00"
    numeric    = ""
    for ch in rearranged:
        numeric += (isLetter(ch) ? str(charCode(upper(ch)) - 55) : ch)
    check       = 98 - mod97(numeric)
    return countryCode + zeroPad2(check) + bban
```

> **Verified.** The generator round-trips: synthesizing from `("DE","370400440532013000")`, `("GB","WEST12345698765432")`, `("NL","ABNA0417164300")` produced IBANs the validator accepts, and the computed check digits exactly reproduced the published ones — GB→`82`, DE→`89`, NL→`91`, CH→`93`.

### Caveats (read before seeding demo data)

- **No official test/reserved IBAN range.** Unlike RFC 5737 documentation IP ranges or issuer test-card BINs, there is **no ISO/SWIFT-reserved "test" IBAN space**. `GB82WEST12345698765432` and `DE89370400440532013000` are merely *documentation examples*, not sandboxes. Vendor "test IBAN" lists ([iban.com/testibans](https://www.iban.com/testibans), [Rapyd](https://docs.rapyd.net/en/iban-numbers-for-testing.html), Wise) are conventions of those platforms, useful as negative-test fixtures, not a standard. ([Wikipedia](https://en.wikipedia.org/wiki/International_Bank_Account_Number))
- **Format-valid ≠ bank-registered.** A generated IBAN passes offline format + checksum validation but is **not registered at any real bank**, so it will fail a live bank/BIC directory lookup. That is exactly what you want for seed/demo data — *and* it means generated IBANs **must never be routed to a real payment rail**. Gate them behind a demo/test mode.
- **Uniqueness & reproducibility.** Random BBANs can collide under a `UNIQUE` constraint. Prefer **deterministic** synthesis from the account sequence/UUID (stable across re-seeds, no collisions), then compute check digits. A purely numeric-BBAN country (NO, DE) simplifies synthesis vs GB (4-letter bank code).
- **Don't reuse published examples** (`GB82WEST…`, `DE89…`) as live demo accounts — some validators flag known examples.

Mature libraries exist if you prefer not to hand-roll BBAN format masks: Go — [`github.com/jacoelho/banking/iban`](https://github.com/jacoelho/banking) (`iban.Generate(cc)`, v1.9.1 Dec 2025, MIT, registry-generated); JS — [`ibantools`](https://github.com/Simplify/ibantools) (`composeIBAN`) or `ibankit` (`IBAN.random()`); Python — [`schwifty`](https://schwifty.readthedocs.io/) (`IBAN.random` / `IBAN.generate`).

---

## 4. Per-country length table (verified)

Full ISO 13616 / SWIFT IBAN Registry (Release 99, December 2024 — current at time of writing). All 89 registry lengths cross-validated and identical across the SWIFT registry, the Wikipedia ISO 13616 table, and iban.com/structure. ✓ marks the ~30 countries that embed an *additional national* check digit (informational — do not re-validate). ([SWIFT IBAN Registry](https://www.swift.com/standards/data-standards/iban-international-bank-account-number); [Wikipedia](https://en.wikipedia.org/wiki/International_Bank_Account_Number); [iban.com/structure](https://www.iban.com/structure))

| CC | Len | Nat.chk | Country |
|----|----:|:-------:|---------|
| AL | 28 | ✓ | Albania |
| AD | 24 |  | Andorra |
| AT | 20 |  | Austria |
| AZ | 28 |  | Azerbaijan |
| BH | 22 |  | Bahrain |
| BY | 28 |  | Belarus |
| BE | 16 | ✓ | Belgium |
| BA | 20 | ✓ | Bosnia and Herzegovina (chk `39`) |
| BR | 29 |  | Brazil |
| BG | 22 |  | Bulgaria |
| BI | 27 |  | Burundi |
| CR | 22 |  | Costa Rica |
| HR | 21 |  | Croatia |
| CY | 28 |  | Cyprus |
| CZ | 24 | ✓ | Czechia |
| DK | 18 | ✓ | Denmark |
| DJ | 27 |  | Djibouti |
| DO | 28 |  | Dominican Republic |
| TL | 23 | ✓ | Timor-Leste (chk `38`) |
| EG | 29 |  | Egypt |
| SV | 28 |  | El Salvador |
| EE | 20 | ✓ | Estonia |
| FK | 18 |  | Falkland Islands |
| FO | 18 | ✓ | Faroe Islands |
| FI | 18 | ✓ | Finland (also AX) |
| FR | 27 | ✓ | France (+ FR overseas terr.) |
| GE | 22 |  | Georgia |
| DE | 22 |  | Germany |
| GI | 23 |  | Gibraltar |
| GR | 27 |  | Greece |
| GL | 18 | ✓ | Greenland |
| GT | 28 |  | Guatemala |
| HN | 28 |  | Honduras (added Rel. 99) |
| HU | 28 | ✓ | Hungary |
| IS | 26 | ✓ | Iceland |
| IQ | 23 |  | Iraq |
| IE | 22 |  | Ireland |
| IL | 23 |  | Israel |
| IT | 27 | ✓ | Italy (CIN) |
| JO | 30 |  | Jordan |
| KZ | 20 |  | Kazakhstan |
| XK | 20 | ✓ | Kosovo |
| KW | 30 |  | Kuwait |
| LV | 21 |  | Latvia |
| LB | 28 |  | Lebanon |
| LY | 25 |  | Libya |
| LI | 21 |  | Liechtenstein |
| LT | 20 |  | Lithuania |
| LU | 20 |  | Luxembourg |
| MT | 31 |  | Malta |
| MR | 27 | ✓ | Mauritania (chk `13`) |
| MU | 30 |  | Mauritius |
| MC | 27 | ✓ | Monaco |
| MD | 24 |  | Moldova |
| MN | 20 |  | Mongolia |
| ME | 22 | ✓ | Montenegro (chk `25`) |
| NL | 18 | ✓ | Netherlands (elfproef) |
| NI | 28 |  | Nicaragua (updated Rel. 99) |
| MK | 19 | ✓ | North Macedonia (chk `07`) |
| NO | 15 | ✓ | Norway (shortest) |
| OM | 23 |  | Oman |
| PK | 24 |  | Pakistan |
| PS | 29 |  | Palestinian territories |
| PL | 28 | ✓ | Poland |
| PT | 25 | ✓ | Portugal (chk `50`) |
| QA | 29 |  | Qatar |
| RO | 24 |  | Romania |
| RU | 33 |  | Russia (longest in registry) |
| LC | 32 |  | Saint Lucia |
| SM | 27 | ✓ | San Marino (CIN) |
| ST | 25 |  | Sao Tome and Principe |
| SA | 24 |  | Saudi Arabia |
| RS | 22 | ✓ | Serbia (chk `35`) |
| SC | 31 |  | Seychelles |
| SK | 24 | ✓ | Slovakia |
| SI | 19 | ✓ | Slovenia (chk `56`) |
| SO | 23 |  | Somalia |
| ES | 24 | ✓ | Spain (DC) |
| SD | 18 |  | Sudan |
| SE | 24 | ✓ | Sweden |
| CH | 21 |  | Switzerland |
| TN | 24 | ✓ | Tunisia (chk `59`) |
| TR | 26 |  | Turkey |
| UA | 29 |  | Ukraine |
| AE | 23 |  | United Arab Emirates |
| GB | 22 |  | United Kingdom (+ IM, JE, GG) |
| VA | 22 |  | Vatican City |
| VG | 24 |  | British Virgin Islands |
| YE | 30 |  | Yemen |

Range: **NO=15 (shortest) → RU=33 (longest in registry)**; the ISO 13616 maximum permitted is 34. The ~22 "experimental"/non-registry codes that aggregators list (AO, BF, BJ, CI, CM, DZ, MA, ML, SN, …) and unconfirmed entries (KM, MG) are deliberately excluded — treat them as a separate lower-trust list only if a broader definition of "IBAN-participating" is needed. ([iban.com/structure](https://www.iban.com/structure))

> **Verified.** A 38-country subset was cross-checked four ways — the in-code go-iban table, iban.com/structure, Wikipedia, and *empirical character-length measurement of real sample IBAN files* — with **zero discrepancies**. The full registry table above is the basis for the strict `COUNTRY_LENGTHS` map in §2/§6.

---

## 5. Where to validate in bank0

Validate at every layer with a clear **authority split**: the client is convenience only; **Go and Postgres are the two authorities** and both must enforce the MOD-97 checksum, not just a format regex.

| Layer | File(s) | Role | What it does |
|---|---|---|---|
| Client (Preact/TS) | `web/app/src/lib/iban.ts`, `web/app/src/routes/Transfer.tsx` | UX only — *never* authority | Instant inline "invalid IBAN" hint; gate the Look-up / Save buttons |
| Go server | `internal/iban`, `internal/api/handlers_beneficiaries.go`, `handlers_accounts.go` | **Authority #1** — fail fast, clean `422` | Reject bad checksum before touching the DB, with a precise message via `writeError` |
| PostgreSQL | `00002_iban.sql` (functions), `00007_accounts.sql` / `00011_beneficiaries.sql` (CHECKs) | **Authority #2** — non-bypassable backstop | `IMMUTABLE` plpgsql MOD-97 in a `CHECK`; protects admin console, seeds, migrations, any future writer |

Both authorities enforce the checksum: `internal/iban` (Go, fail-fast
`422 invalid_iban`) and the `iban_is_valid()` MOD-97 validator behind `CHECK`s on
`accounts.iban` and `beneficiaries.iban` (Postgres backstop, `23514` → `422`).
`web/app/src/lib/iban.ts` is the convenience-only client check. See §6 for the
migration breakdown.

**Why both layers.** A format regex is **necessary but not sufficient** (it cannot
catch a transposed digit), so the checksum lives in at least one *authority*. For a
core-banking ledger the answer is **both**: Go (fail-fast, good errors) **and**
Postgres (last line of defense) — defense in depth with the DB as the
non-bypassable backstop, honoring bank0's "logic lives in the database" and
"the DB is the source of truth" rules. A `23514` check-violation flows cleanly
through `mapDBError` in `internal/api/respond.go` (→ `422`).

**Postgres cost assessment — cheap.** The MOD-97 validator is a pure function of its argument with no DB reads, so it can be marked **`IMMUTABLE PARALLEL SAFE`**; Postgres already assumes `CHECK` conditions are immutable, so a pure validator fits the model perfectly, and a `DOMAIN` caches the constraint lookup per session. It is a **bounded loop over ≤34 chars** doing small modular arithmetic (the accumulator stays `< 97` — no bignum), so it is trivially cheap for per-insert enforcement.

> **Benchmarked** on Postgres 18.4: ~0.1 µs/call (1M calls on 27/31-char IBANs), in the noise versus INSERT/trigger cost — cheap enough for per-insert enforcement.

---

## 6. Where it lives in the baseline

The `iban_is_valid` / `iban_generate` MOD-97 functions, the per-country length
table as a shared `iban_country_length()` helper, the unregistered-country reject,
and the BBAN-length guard in `iban_generate` all live in
[`00002_iban.sql`](../db/migrations/00002_iban.sql). The matching CHECK constraints
sit alongside their tables: `accounts.iban` in
[`00004_accounts.sql`](../db/migrations/00004_accounts.sql) and
`beneficiaries.iban` in
[`00008_features.sql`](../db/migrations/00008_features.sql). All three layers —
`internal/iban`, the DB migrations, and `web/app/src/lib/iban.ts` — carry the
**same 89-country table** and agree byte-for-byte. Column CHECKs are used rather
than a shared `DOMAIN`. DB-layer coverage lives in `internal/db/iban_test.go`; the
demo seed mints deterministic, checksum-valid-but-unregistered IBANs via
`iban.Generate`, fenced to demo mode.

---

### Sources

- ISO 13616 / IBAN structure, MOD-97 algorithm, worked example: https://en.wikipedia.org/wiki/International_Bank_Account_Number
- Per-country structure & length table (ISO 13616 registry mirror): https://www.iban.com/structure
- SWIFT IBAN Registry (Release 99, Dec 2024): https://www.swift.com/standards/data-standards/iban-international-bank-account-number
- MOD-97 implementation walkthrough: https://fakemyinfo.com/guides/iban-validation-mod97-explained.html
- Generation write-up: https://medium.com/@matlabb/iban-structure-and-mod-97-validation-algorithm-719e3d4db5f2
- Test-IBAN fixtures (vendor conventions, not a standard): https://www.iban.com/testibans · https://docs.rapyd.net/en/iban-numbers-for-testing.html
- Libraries: https://github.com/jacoelho/banking (Go) · https://github.com/Simplify/ibantools + https://www.npmjs.com/package/ibantools (JS/TS) · https://schwifty.readthedocs.io/ (Python)
- Postgres `IMMUTABLE`/volatility: https://aws.amazon.com/blogs/database/volatility-classification-in-postgresql/ · https://www.cybertec-postgresql.com/en/functions-the-most-widely-ignored-performance-tweak/
- bank0 source: `db/migrations/00002_iban.sql`, `internal/iban`, `internal/api/handlers_beneficiaries.go`, `internal/api/handlers_accounts.go`, `internal/api/respond.go`, `web/app/src/lib/iban.ts`, `web/app/src/routes/Transfer.tsx`
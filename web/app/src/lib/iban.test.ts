import { describe, it, expect } from "vitest";
import { isValidIBAN, formatIBAN, COUNTRY_LENGTHS } from "./iban";

// The per-country length table is cross-checked against the Go + SQL copies by a
// Go test (internal/iban/drift_test.go). These tests cover the TS algorithm itself.
describe("isValidIBAN", () => {
  it("accepts well-formed IBANs, ignoring spaces and case", () => {
    expect(isValidIBAN("GB82 WEST 1234 5698 7654 32")).toBe(true);
    expect(isValidIBAN("de89 3704 0044 0532 0130 00")).toBe(true);
  });
  it("rejects a bad checksum", () => {
    expect(isValidIBAN("GB82WEST12345698765431")).toBe(false);
  });
  it("rejects the wrong length for the country", () => {
    expect(isValidIBAN("GB82WEST1234569876543")).toBe(false); // one short
  });
  it("rejects an unregistered country code", () => {
    expect(isValidIBAN("ZZ000000000000000000")).toBe(false);
  });
  it("rejects malformed structure", () => {
    expect(isValidIBAN("")).toBe(false);
    expect(isValidIBAN("GB!!WEST12345698765432")).toBe(false);
  });
});

describe("formatIBAN", () => {
  it("groups into blocks of four", () => {
    expect(formatIBAN("GB82WEST12345698765432")).toBe("GB82 WEST 1234 5698 7654 32");
  });
});

describe("COUNTRY_LENGTHS", () => {
  it("covers the SEPA basics", () => {
    expect(COUNTRY_LENGTHS.GB).toBe(22);
    expect(COUNTRY_LENGTHS.DE).toBe(22);
    expect(COUNTRY_LENGTHS.NO).toBe(15);
  });
});

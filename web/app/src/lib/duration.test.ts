import { describe, it, expect } from "vitest";
import { formatCountdown } from "./duration";

describe("formatCountdown", () => {
  it("pads minutes and seconds to two digits", () => {
    expect(formatCountdown(0)).toBe("00:00");
    expect(formatCountdown(5)).toBe("00:05");
    expect(formatCountdown(65)).toBe("01:05");
    expect(formatCountdown(600)).toBe("10:00");
  });
  it("clamps negatives to zero", () => {
    expect(formatCountdown(-3)).toBe("00:00");
  });
  it("floors fractional seconds", () => {
    expect(formatCountdown(9.9)).toBe("00:09");
  });
  it("allows minutes beyond 99", () => {
    expect(formatCountdown(6000)).toBe("100:00");
  });
});

import { describe, it, expect } from "vitest";
import { coolingRemaining, sendState, statusActions } from "./fraudGate";
import type { TransferDecision, TransferIntent } from "../api/types";

// Small builders so each case reads as "an intent with these traits".
function intent(
  decision: TransferDecision,
  warning?: { required_ack: boolean; cooling_off_seconds?: number } | null,
): TransferIntent {
  return {
    decision,
    risk_band: "high",
    reason_codes: [],
    warning: warning
      ? {
          warning_id: "w",
          category: "risk_warning",
          severity: decision === "block" ? "critical" : "warning",
          headline: "h",
          body: "b",
          required_ack: warning.required_ack,
          cooling_off_seconds: warning.cooling_off_seconds ?? 0,
        }
      : null,
    step_up_method: null,
  };
}

describe("coolingRemaining", () => {
  const cases: Array<[string, number, number | null, number, number]> = [
    // [name, coolingOffSeconds, ackedAtMs, nowMs, expected]
    ["null ack -> 0", 30, null, 1_000_000, 0],
    ["zero window -> 0", 0, 1_000_000, 1_000_000, 0],
    ["negative window -> 0", -5, 1_000_000, 1_000_000, 0],
    ["just acked -> full window", 30, 1_000_000, 1_000_000, 30],
    ["ceil rounds up partial second", 30, 1_000_000, 1_000_500, 30], // 0.5s elapsed -> ceil(29.5)=30
    ["mid countdown ceils", 30, 1_000_000, 1_010_100, 20], // 10.1s elapsed -> ceil(19.9)=20
    ["exactly at window boundary -> 0", 30, 1_000_000, 1_030_000, 0], // 30s elapsed -> ceil(0)=0
    ["past window clamps to 0", 30, 1_000_000, 1_099_000, 0], // 99s elapsed -> max(0, negative)
    ["one second in", 5, 1_000_000, 1_001_000, 4],
  ];
  for (const [name, cool, ackedAt, now, want] of cases) {
    it(name, () => {
      expect(coolingRemaining(cool, ackedAt, now)).toBe(want);
    });
  }
});

describe("sendState", () => {
  const cases: Array<
    [string, TransferIntent | null, boolean, number, ReturnType<typeof sendState>]
  > = [
    // [name, intent, acked, coolLeft, expected]
    [
      "no intent (preflight failed) -> enabled, no ack",
      null,
      false,
      0,
      { mode: "enabled", needsAck: false, counting: false },
    ],
    [
      "allow decision -> enabled",
      intent("allow"),
      false,
      0,
      { mode: "enabled", needsAck: false, counting: false },
    ],
    [
      "warn with no required_ack -> enabled, no ack",
      intent("warn", { required_ack: false }),
      false,
      0,
      { mode: "enabled", needsAck: false, counting: false },
    ],
    [
      "warn with no warning object -> enabled",
      intent("warn", null),
      false,
      0,
      { mode: "enabled", needsAck: false, counting: false },
    ],
    [
      "required_ack unticked -> disabled awaiting ack",
      intent("warn", { required_ack: true, cooling_off_seconds: 30 }),
      false,
      0,
      { mode: "disabled", needsAck: true, counting: false },
    ],
    [
      "required_ack ticked, cooling 0 -> enabled immediately",
      intent("warn", { required_ack: true, cooling_off_seconds: 0 }),
      true,
      0,
      { mode: "enabled", needsAck: true, counting: false },
    ],
    [
      "required_ack ticked, mid countdown -> disabled counting",
      intent("warn", { required_ack: true, cooling_off_seconds: 30 }),
      true,
      12,
      { mode: "disabled", needsAck: true, counting: true },
    ],
    [
      "required_ack ticked, boundary coolLeft==0 -> enabled",
      intent("warn", { required_ack: true, cooling_off_seconds: 30 }),
      true,
      0,
      { mode: "enabled", needsAck: true, counting: false },
    ],
    [
      "block always hidden even with required_ack unticked",
      intent("block", { required_ack: true, cooling_off_seconds: 30 }),
      false,
      0,
      { mode: "hidden", needsAck: false, counting: false },
    ],
    [
      "block hidden even if acked and counting values present",
      intent("block", { required_ack: true, cooling_off_seconds: 30 }),
      true,
      12,
      { mode: "hidden", needsAck: false, counting: false },
    ],
    [
      "review decision (no required_ack) does not disable send",
      intent("review", { required_ack: false }),
      false,
      0,
      { mode: "enabled", needsAck: false, counting: false },
    ],
    [
      "step_up decision is sendable",
      intent("step_up", null),
      false,
      0,
      { mode: "enabled", needsAck: false, counting: false },
    ],
  ];
  for (const [name, i, acked, coolLeft, want] of cases) {
    it(name, () => {
      expect(sendState(i, acked, coolLeft)).toEqual(want);
    });
  }
});

describe("statusActions", () => {
  const cases: Array<[string, string, ReturnType<typeof statusActions>]> = [
    // held -> Confirm and send + Cancel payment
    ["held -> confirm + cancel", "held", { held: true, underReview: false, posted: false, disputable: false }],
    // under_review -> info card, no actions
    ["under_review -> no actions", "under_review", { held: false, underReview: true, posted: false, disputable: false }],
    // posted -> dispute-eligible
    ["posted -> dispute-eligible", "posted", { held: false, underReview: false, posted: true, disputable: true }],
    // pending (and any other status) -> plain status line only, no actions
    ["pending -> no actions", "pending", { held: false, underReview: false, posted: false, disputable: false }],
    ["canceled -> no actions", "canceled", { held: false, underReview: false, posted: false, disputable: false }],
  ];
  for (const [name, status, want] of cases) {
    it(name, () => {
      expect(statusActions(status)).toEqual(want);
    });
  }
});

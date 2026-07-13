// Pure decision logic for the client fraud-gate UI, extracted from the Transfer
// confirm screen and the Receipt held/review branches so it can be unit-tested in
// isolation. These functions carry NO React/Preact state — they mirror exactly
// what the components compute inline, and the components call them so the two
// never drift.
import type { TransferIntent } from "../api/types";

// Seconds left on the cooling-off countdown, given the cooling-off window, the ms
// timestamp the ack posted (null = not acked), and "now" in ms. Returns 0 when
// there is nothing to count (no ack yet, or no/zero cooling-off window), and
// otherwise Math.ceil of the remaining seconds, floored at 0 — matching the
// component's tick() exactly: Math.max(0, Math.ceil(cool - (now - ackedAt)/1000)).
export function coolingRemaining(
  coolingOffSeconds: number,
  ackedAtMs: number | null,
  nowMs: number,
): number {
  if (ackedAtMs == null || coolingOffSeconds <= 0) return 0;
  return Math.max(0, Math.ceil(coolingOffSeconds - (nowMs - ackedAtMs) / 1000));
}

// How the confirm-screen Send button should present, independent of the transient
// `busy` flag (which the component ANDs in on top: a busy enabled button still
// shows "Sending…" and is disabled). `acked` is `ackedAt != null`; `coolLeft` is
// the current countdown value (see coolingRemaining).
//
//   mode "hidden"   -> decision is block; the button isn't rendered at all
//   mode "disabled" -> awaiting the ack tick, or counting down the cooling-off
//   mode "enabled"  -> sendable (server still makes the final call)
//   needsAck        -> render the "I understand" checkbox
//   counting        -> cooling-off is actively ticking (checkbox locked, "Wait …")
export interface SendState {
  mode: "hidden" | "disabled" | "enabled";
  needsAck: boolean;
  counting: boolean;
}

export function sendState(
  intent: TransferIntent | null,
  acked: boolean,
  coolLeft: number,
): SendState {
  const warning = intent?.warning ?? null;
  const blocked = intent?.decision === "block";
  const needsAck = !!warning?.required_ack && !blocked;
  const counting = needsAck && acked && coolLeft > 0;
  let mode: SendState["mode"];
  if (blocked) mode = "hidden";
  else if (needsAck && !acked) mode = "disabled"; // awaiting ack
  else if (counting) mode = "disabled"; // cooling-off ticking
  else mode = "enabled";
  return { mode, needsAck, counting };
}

// Which receipt affordances a transfer status unlocks. Mirrors the Receipt locals:
//   held        -> render the held card (Confirm and send / Cancel payment)
//   underReview -> render the under-review info card (no actions)
//   posted      -> money moved
//   disputable  -> show "Report a problem" (currently: posted only)
// Every other status (e.g. "pending", "canceled") yields all-false and only the
// plain status line renders.
export interface ReceiptStatusActions {
  held: boolean;
  underReview: boolean;
  posted: boolean;
  disputable: boolean;
}

export function statusActions(status: string): ReceiptStatusActions {
  const posted = status === "posted";
  const held = status === "held";
  const underReview = status === "under_review";
  return { held, underReview, posted, disputable: posted };
}

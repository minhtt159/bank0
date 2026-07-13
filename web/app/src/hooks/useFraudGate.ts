import { useEffect, useState } from "preact/hooks";
import { api, ApiError } from "../api/client";
import { coolingRemaining, sendState } from "../lib/fraudGate";
import type { TransferIntent } from "../api/types";

// Ack + cooling-off state for the fraud-gate warning card on the confirm screen.
// Owns the ack timestamp, the POST to /me/warning-acks, and the 250ms countdown;
// the pure gate decision stays in lib/fraudGate so the two never drift.
export function useFraudGate(
  intent: TransferIntent | null,
  ctx: {
    debitAccountId: string | undefined;
    counterpartyIban: string | undefined;
    amountMinor: number | null;
    onError: (msg: string) => void;
  },
) {
  const [ackedAt, setAckedAt] = useState<number | null>(null); // ms when the ack posted
  const [ackBusy, setAckBusy] = useState(false);
  const [coolLeft, setCoolLeft] = useState(0);

  // Run the cooling-off countdown off the ack timestamp so it survives re-renders
  // and stays accurate regardless of tick jitter.
  useEffect(() => {
    const cool = intent?.warning?.cooling_off_seconds ?? 0;
    if (ackedAt == null || cool <= 0) {
      setCoolLeft(0);
      return;
    }
    const tick = () => setCoolLeft(coolingRemaining(cool, ackedAt, Date.now()));
    tick();
    const iv = setInterval(tick, 250);
    return () => clearInterval(iv);
  }, [ackedAt, intent]);

  // Pure gate decision (see lib/fraudGate). step_up/review/warn/allow are all
  // sendable — the server makes the final call (e.g. 403 step_up_required flows to
  // the banner). The component layers its transient `busy` flag on top.
  const gate = sendState(intent, ackedAt != null, coolLeft);

  async function toggleAck(checked: boolean) {
    if (!checked) {
      setAckedAt(null);
      setCoolLeft(0);
      return;
    }
    const warning = intent?.warning;
    if (!warning || !ctx.debitAccountId || !ctx.counterpartyIban || ctx.amountMinor == null) return;
    setAckBusy(true);
    ctx.onError("");
    try {
      // Record the liability evidence, THEN start the cooling-off clock. Server-side
      // the ack must age >= cooling_off_seconds before the submit is accepted.
      await api.recordWarningAck({
        category: warning.category,
        reason_code: intent?.reason_codes?.[0],
        acknowledged: true,
        debit_account_id: ctx.debitAccountId,
        counterparty_iban: ctx.counterpartyIban,
        amount_minor: ctx.amountMinor,
        device: "pwa",
      });
      setAckedAt(Date.now());
    } catch (e) {
      ctx.onError(e instanceof ApiError ? e.message : "Could not record your acknowledgement — try again.");
    } finally {
      setAckBusy(false);
    }
  }

  // A new review() or a rejected submit forces a fresh ack + cooling-off.
  function reset() {
    setAckedAt(null);
    setCoolLeft(0);
  }

  return { gate, acked: ackedAt != null, ackBusy, coolLeft, toggleAck, reset };
}

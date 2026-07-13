import { useEffect, useMemo, useState } from "preact/hooks";
import { useLocation } from "preact-iso";
import { api, ApiError } from "../api/client";
import { userId } from "../store/auth";
import { formatMinor, parseMajor } from "../lib/money";
import { fuzzyFilter } from "../lib/fuzzy";
import { formatCountdown } from "../lib/duration";
import { useFraudGate } from "../hooks/useFraudGate";
import { AddPayeePanel } from "../components/AddPayeePanel";
import type {
  Account,
  Beneficiary,
  TransferDecision,
  TransferIntent,
  TransferSuggestion,
  WarningSeverity,
} from "../api/types";
import { ErrorBanner } from "../lib/feedback";

function severityLabel(s: WarningSeverity): string {
  return s === "critical" ? "Important" : s === "warning" ? "Warning" : "Please note";
}

// Build a warning card from a submit-time 409/422 when we have no preflight result
// to reuse — so a blocked/ack-required error renders the same UI as the preflight.
function synthIntent(decision: TransferDecision, message: string, requiredAck: boolean): TransferIntent {
  return {
    decision,
    risk_band: "high",
    reason_codes: [],
    warning: {
      warning_id: "",
      category: "risk_warning",
      severity: decision === "block" ? "critical" : "warning",
      headline: decision === "block" ? "This payment can't be sent" : "Please confirm before sending",
      body: message,
      required_ack: requiredAck,
      cooling_off_seconds: 0,
    },
    step_up_method: null,
  };
}

export function Transfer() {
  const { route } = useLocation();
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [bens, setBens] = useState<Beneficiary[]>([]);
  const [err, setErr] = useState("");

  const [srcId, setSrcId] = useState("");
  const [srcQ, setSrcQ] = useState("");
  const [dstId, setDstId] = useState("");
  const [dstQ, setDstQ] = useState("");
  const [amount, setAmount] = useState("");

  const [step, setStep] = useState<"form" | "confirm">("form");
  const [idemKey, setIdemKey] = useState("");
  const [busy, setBusy] = useState(false);

  // Fraud preflight (POST /transfers/intent) result for the confirm screen; the
  // acknowledgement + cooling-off state its warning card drives lives in useFraudGate.
  const [intent, setIntent] = useState<TransferIntent | null>(null);

  // Guided-transfer demo suggestion (read-only; never moves money). Dismissed once
  // applied or rejected so it doesn't nag.
  const [suggestion, setSuggestion] = useState<TransferSuggestion | null>(null);
  const [suggestDismissed, setSuggestDismissed] = useState(false);

  useEffect(() => {
    api.accounts(userId.value).then((a) => {
      setAccounts(a);
      const def = a.find((x) => x.is_default) ?? a[0];
      if (def) setSrcId(def.id);
    }).catch((e) => setErr(e.message));
    api.beneficiaries().then(setBens).catch((e) => setErr(e.message));
  }, []);

  // Guided-transfer "mule menu": fetch up to 3 candidate payees as the source /
  // amount change, then pick ONE at random to present. If the menu is empty, fall
  // back to the customer's own other account (the safe stand-in). Failures are
  // silent — it's a hint, not a blocker.
  useEffect(() => {
    if (!srcId) return;
    const m = parseMajor(amount) ?? undefined;
    api.transferSuggestions(srcId, m).then((menu) => {
      if (menu.length > 0) {
        setSuggestion(menu[Math.floor(Math.random() * menu.length)]);
        return;
      }
      const own = accounts.find((a) => a.id !== srcId && !!a.iban);
      setSuggestion(
        own
          ? {
              account_id: own.id,
              iban: own.iban as string,
              owner_name_masked: "Your account",
              reason: "your account",
              source: "own_account",
            }
          : null,
      );
    }).catch(() => setSuggestion(null));
  }, [srcId, amount, accounts]);

  const src = accounts.find((a) => a.id === srcId);
  const dst = bens.find((b) => b.id === dstId);
  const minor = parseMajor(amount);
  const overBalance = src != null && minor != null && minor > src.available_minor;

  const srcMatches = useMemo(
    () => fuzzyFilter(accounts, srcQ, (a) => `${a.iban} ${a.kind}`),
    [accounts, srcQ],
  );
  const dstMatches = useMemo(
    () => fuzzyFilter(bens, dstQ, (b) => `${b.label} ${b.iban} ${b.owner_name_masked}`),
    [bens, dstQ],
  );

  const { gate, acked, ackBusy, coolLeft, toggleAck, reset: resetGate } = useFraudGate(intent, {
    debitAccountId: src?.id,
    counterpartyIban: dst?.iban,
    amountMinor: minor,
    onError: setErr,
  });

  const warning = intent?.warning ?? null;
  const blocked = gate.mode === "hidden";
  const needsAck = gate.needsAck;
  const coolingActive = gate.counting;
  const canSend = !busy && gate.mode === "enabled";

  async function review() {
    setErr("");
    if (!src || !dst || minor == null) {
      setErr("Choose a source, a payee, and a valid amount.");
      return;
    }
    if (overBalance) {
      setErr("Amount exceeds available balance.");
      return;
    }
    setIdemKey(crypto.randomUUID()); // one key per attempt; reused on retry below
    setIntent(null);
    resetGate();
    setStep("confirm");
    // Read-only fraud preflight. Non-blocking: if it errors we proceed exactly as
    // before — the submit path still enforces the gates server-side.
    try {
      const res = await api.transferIntent({
        debit_account: src.id,
        credit_account: dst.credit_account_id,
        amount_minor: minor,
      });
      setIntent(res);
    } catch {
      /* silent — preflight is advisory */
    }
  }

  async function send() {
    if (!src || !dst || minor == null) return;
    setBusy(true);
    setErr("");
    try {
      const res = await api.createTransfer(
        { debit_account: src.id, credit_account: dst.credit_account_id, amount_minor: minor },
        idemKey,
      );
      // held / under_review route to the receipt just like posted/pending.
      route(`/transfer/${res.transfer_id}`, true);
    } catch (e) {
      // Map the two fraud-gate rejections to the same warning UI rather than a raw
      // banner; keep every other error (incl. 403 step_up_required) as-is.
      if (e instanceof ApiError && e.code === "payment_blocked") {
        setIntent(synthIntent("block", e.message, false));
      } else if (e instanceof ApiError && e.code === "ack_required") {
        // No reusable preflight warning (call failed or rules changed since):
        // re-fetch it so the card carries the real copy + cooling-off, falling
        // back to a synthesized card. Server enforces either way.
        if (!intent?.warning?.required_ack) {
          let fresh: TransferIntent | null = null;
          try {
            fresh = await api.transferIntent({
              debit_account: src.id,
              credit_account: dst.credit_account_id,
              amount_minor: minor,
            });
          } catch {
            /* fall through to synth */
          }
          setIntent(fresh?.warning?.required_ack ? fresh : synthIntent("warn", e.message, true));
        }
        resetGate(); // force a fresh ack + cooling-off
      } else {
        setErr(e instanceof ApiError ? e.message : "Transfer failed");
      }
      setBusy(false); // keep idemKey so a retry of this attempt dedupes
    }
  }

  if (err && accounts.length === 0) return <ErrorBanner>{err}</ErrorBanner>;

  if (step === "confirm" && src && dst && minor != null) {
    return (
      <>
        <button type="button" class="link" style="color:var(--muted)" onClick={() => setStep("form")}>‹ Edit</button>
        <h1>Confirm transfer</h1>
        <div class="card">
          <div class="amount" style="text-align:center;margin:8px 0 16px">{formatMinor(minor, src.currency)}</div>
          <div class="row"><span class="muted">From</span><span class="iban">{src.iban}</span></div>
          <div class="row"><span class="muted">To</span><span>{dst.label}</span></div>
          <div class="row"><span class="muted">Payee name</span><span>{dst.owner_name_masked}</span></div>
          <div class="row"><span class="muted">Payee IBAN</span><span class="iban">{dst.iban}</span></div>
        </div>

        {warning && (
          <div class={`warn-card ${warning.severity}`}>
            <div
              role={warning.severity === "critical" ? "alert" : undefined}
              aria-live={warning.severity === "critical" ? undefined : "polite"}
            >
              <div class="warn-tag">{severityLabel(warning.severity)}</div>
              <strong>{warning.headline}</strong>
              <p style="margin:6px 0 0">{warning.body}</p>
              {intent?.decision === "review" && !blocked && (
                <p class="muted" style="margin:8px 0 0">
                  If you send this, we'll hold it for a short review before the money moves.
                </p>
              )}
            </div>

            {needsAck && (
              <label class="ack-row">
                <input
                  type="checkbox"
                  checked={acked}
                  disabled={ackBusy || coolingActive}
                  onChange={(e) => toggleAck((e.target as HTMLInputElement).checked)}
                />
                <span>I understand the risk and want to proceed.</span>
              </label>
            )}

            {coolingActive && (
              <p class="muted" aria-live="polite" style="margin:8px 0 0">
                Please wait <strong>{formatCountdown(coolLeft)}</strong> before you can send.
              </p>
            )}
          </div>
        )}

        {err && <ErrorBanner>{err}</ErrorBanner>}

        {blocked ? (
          <p class="muted">
            Go back to change the payee or amount, or contact us if you think this is a mistake.
          </p>
        ) : (
          <button class="block" onClick={send} disabled={!canSend}>
            {busy
              ? "Sending…"
              : coolingActive
                ? `Wait ${formatCountdown(coolLeft)}`
                : `Send ${formatMinor(minor, src.currency)}`}
          </button>
        )}
      </>
    );
  }

  return (
    <>
      <a class="muted" href="/">‹ Cancel</a>
      <h1>Send money</h1>
      {err && <ErrorBanner>{err}</ErrorBanner>}

      <h2>From</h2>
      <input placeholder="Search your accounts" aria-label="Search your accounts" value={srcQ}
        onInput={(e) => setSrcQ((e.target as HTMLInputElement).value)} />
      <div style="margin-top:8px" role="radiogroup" aria-label="Source account">
        {srcMatches.map((a) => (
          <label key={a.id} class={`pick ${a.id === srcId ? "sel" : ""}`}>
            <input type="radio" name="source-account" class="visually-hidden"
              checked={a.id === srcId} onChange={() => setSrcId(a.id)} />
            <div class="row">
              <span class="iban">{a.iban}</span>
              <span>{formatMinor(a.available_minor, a.currency)}</span>
            </div>
          </label>
        ))}
      </div>

      <h2 style="margin-top:18px">To</h2>
      <AddPayeePanel onSaved={(b) => { setBens((cur) => [...cur, b]); setDstId(b.id); }}>
        {(openWith) => (
          <>
            {suggestion && !suggestDismissed && (
              <div class="card" style="border-color:var(--accent)">
                <div class="row">
                  <span>{suggestion.reason || "Suggested payee"}</span>
                  <button class="link" style="color:var(--accent)"
                    onClick={() => setSuggestDismissed(true)}>Dismiss</button>
                </div>
                <div style="margin:6px 0"><strong>{suggestion.owner_name_masked}</strong></div>
                <div class="iban muted" style="font-size:13px">{suggestion.iban}</div>
                {/* Route the suggestion through the existing "add payee" flow: pre-fill
                    the IBAN and label, then resolve it (confirmation of payee). The
                    customer reviews the masked owner before saving, exactly like a
                    manual payee. */}
                <button class="ghost block" style="margin-top:10px" onClick={() => {
                  openWith({ iban: suggestion.iban, label: suggestion.owner_name_masked || "Suggested payee" });
                  setSuggestDismissed(true);
                }}>
                  Use this payee
                </button>
              </div>
            )}
            <input placeholder="Search saved payees" aria-label="Search saved payees" value={dstQ}
              onInput={(e) => setDstQ((e.target as HTMLInputElement).value)} />
            <div style="margin-top:8px" role="radiogroup" aria-label="Payee">
              {dstMatches.map((b) => (
                <label key={b.id} class={`pick ${b.id === dstId ? "sel" : ""}`}>
                  <input type="radio" name="payee" class="visually-hidden"
                    checked={b.id === dstId} onChange={() => setDstId(b.id)} />
                  <div class="row"><span>{b.label}</span><span class="muted">{b.owner_name_masked}</span></div>
                  <div class="iban muted" style="font-size:13px">{b.iban}</div>
                </label>
              ))}
              {bens.length === 0 && <div class="muted">No saved payees yet.</div>}
            </div>
          </>
        )}
      </AddPayeePanel>

      <h2 style="margin-top:18px">Amount</h2>
      <input inputMode="decimal" placeholder="0.00" aria-label="Amount" value={amount}
        onInput={(e) => setAmount((e.target as HTMLInputElement).value)} />
      {overBalance && <ErrorBanner>Exceeds available balance.</ErrorBanner>}

      <button class="block" style="margin-top:20px" onClick={review}
        disabled={!srcId || !dstId || minor == null || overBalance}>
        Review
      </button>
    </>
  );
}

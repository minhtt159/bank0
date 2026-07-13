import { useEffect, useState } from "preact/hooks";
import { api, ApiError } from "../api/client";
import { formatMinor } from "../lib/money";
import { DISPUTE_CATEGORIES, disputeStatusLabel, transferStatusLabel } from "../lib/labels";
import { statusActions } from "../lib/fraudGate";
import type { Dispute, DisputeCategory, Transfer } from "../api/types";
import { ErrorBanner, Loading } from "../lib/feedback";

export function Receipt({ id }: { id: string }) {
  const [t, setT] = useState<Transfer | null>(null);
  const [err, setErr] = useState("");

  // dispute state
  const [disputing, setDisputing] = useState(false);
  const [category, setCategory] = useState<DisputeCategory>("unrecognised");
  const [reason, setReason] = useState("");
  const [raised, setRaised] = useState<Dispute | null>(null);
  const [disputeErr, setDisputeErr] = useState("");
  const [busy, setBusy] = useState(false);

  // held-transfer confirm/cancel actions
  const [actionErr, setActionErr] = useState("");
  const [actionBusy, setActionBusy] = useState(false);

  useEffect(() => {
    api.getTransfer(id).then(setT).catch((e) => setErr(e.message));
  }, [id]);

  async function confirmHold() {
    setActionBusy(true);
    setActionErr("");
    try {
      await api.confirmTransfer(id);
      setT(await api.getTransfer(id)); // re-read to pick up the released status
    } catch (e) {
      setActionErr(e instanceof ApiError ? e.message : "Could not confirm this payment.");
    } finally {
      setActionBusy(false);
    }
  }

  async function cancelHold() {
    setActionBusy(true);
    setActionErr("");
    try {
      await api.cancelTransfer(id);
      setT(await api.getTransfer(id));
    } catch (e) {
      setActionErr(e instanceof ApiError ? e.message : "Could not cancel this payment.");
    } finally {
      setActionBusy(false);
    }
  }

  async function submitDispute(e: Event) {
    e.preventDefault();
    setBusy(true);
    setDisputeErr("");
    try {
      const d = await api.raiseDispute(id, { category, reason: reason.trim() || undefined });
      setRaised(d);
      setDisputing(false);
    } catch (e) {
      setDisputeErr(e instanceof ApiError ? e.message : "Could not report this payment");
    } finally {
      setBusy(false);
    }
  }

  if (err) return <ErrorBanner>{err}</ErrorBanner>;
  if (!t) return <Loading />;

  // A dispute only makes sense once money has actually moved; the server is the
  // authority (422 if not disputable), but hiding the action on non-posted
  // transfers keeps the happy path clean. (see lib/fraudGate.statusActions)
  const { posted, held, underReview, disputable } = statusActions(t.status);

  return (
    <>
      <div class="center" style="padding:20px 0 8px">
        <div class="amount pos">{formatMinor(t.amount_minor, t.currency)}</div>
        <div class="badge" style="margin-top:8px">{transferStatusLabel(t.status)}</div>
      </div>

      {held && (
        <div class="warn-card warning" role="status">
          <div class="warn-tag">Action needed</div>
          <p style="margin:0">
            We've paused this payment for a short cooling-off period so you can double-check it before
            the money moves{t.hold_reason ? ` (${t.hold_reason})` : ""}.
          </p>
          {t.hold_expires_at && (
            <p style="margin:8px 0 0">
              Confirm before <strong>{new Date(t.hold_expires_at).toLocaleString()}</strong> or it will
              be canceled automatically.
            </p>
          )}
          {actionErr && <ErrorBanner>{actionErr}</ErrorBanner>}
          <button class="block" style="margin-top:12px" onClick={confirmHold} disabled={actionBusy}>
            {actionBusy ? "Working…" : "Confirm and send"}
          </button>
          <button class="ghost block" style="margin-top:8px" onClick={cancelHold} disabled={actionBusy}>
            Cancel payment
          </button>
        </div>
      )}

      {underReview && (
        <div class="warn-card info" role="status">
          <div class="warn-tag">Under review</div>
          <p style="margin:0">
            This payment is being reviewed by the bank. No action is needed; we'll release or refuse it
            {t.hold_expires_at ? (
              <> by <strong>{new Date(t.hold_expires_at).toLocaleString()}</strong>.</>
            ) : (
              " shortly."
            )}
          </p>
        </div>
      )}

      <div class="card">
        {!posted && !held && !underReview && (
          <p class="muted">
            This transfer is <strong>{transferStatusLabel(t.status)}</strong>
            {t.status === "pending" ? " — awaiting settlement." : "."}
          </p>
        )}
        <div class="row"><span class="muted">Reference</span><span class="iban">{t.id.slice(0, 8)}</span></div>
        {t.description && <div class="row"><span class="muted">Note</span><span>{t.description}</span></div>}
        {t.posted_at && (
          <div class="row"><span class="muted">Posted</span><span>{new Date(t.posted_at).toLocaleString()}</span></div>
        )}
      </div>

      {raised ? (
        <div class="card">
          <div class="row">
            <span>Problem reported</span>
            <span class="badge">{disputeStatusLabel(raised.status)}</span>
          </div>
          <p class="muted" style="margin:8px 0 0">
            We're looking into it. Track progress in <a href="/disputes">My disputes</a>.
          </p>
        </div>
      ) : disputing ? (
        <form class="card" onSubmit={submitDispute}>
          {disputeErr && <ErrorBanner>{disputeErr}</ErrorBanner>}
          <label for="cat">What's wrong?</label>
          <select id="cat" value={category}
            onChange={(e) => setCategory((e.target as HTMLSelectElement).value as DisputeCategory)}>
            {DISPUTE_CATEGORIES.map((c) => <option value={c.value}>{c.label}</option>)}
          </select>
          <label for="rsn">Details (optional)</label>
          <input id="rsn" value={reason} placeholder="Tell us what happened"
            onInput={(e) => setReason((e.target as HTMLInputElement).value)} />
          <div class="row" style="margin-top:12px;gap:8px">
            <button disabled={busy}>{busy ? "Reporting…" : "Submit report"}</button>
            <button type="button" class="ghost"
              onClick={() => { setDisputing(false); setDisputeErr(""); }}>Cancel</button>
          </div>
        </form>
      ) : (
        disputable && (
          <button class="ghost block" onClick={() => setDisputing(true)}>Report a problem</button>
        )
      )}

      <a class="btn block" href="/" style="text-align:center;display:block;text-decoration:none;margin-top:12px">Back to home</a>
    </>
  );
}

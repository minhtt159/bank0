import { useEffect, useState } from "preact/hooks";
import { api } from "../api/client";
import { formatMinor } from "../lib/money";
import type { Account as Acct, LedgerEntry } from "../api/types";
import { ErrorBanner, Loading } from "../lib/feedback";

const PAGE = 25;

export function Account({ id }: { id: string }) {
  const [acct, setAcct] = useState<Acct | null>(null);
  const [entries, setEntries] = useState<LedgerEntry[]>([]);
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  async function loadMore() {
    if (busy || done) return;
    setBusy(true);
    try {
      const cursor = entries.length ? entries[entries.length - 1].posted_at : undefined;
      const page = await api.ledger(id, cursor, PAGE);
      setEntries((cur) => [...cur, ...page]);
      if (page.length < PAGE) setDone(true);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  useEffect(() => {
    api.account(id).then(setAcct).catch((e) => setErr(e.message));
    loadMore();
  }, [id]);

  if (err) return <ErrorBanner>{err}</ErrorBanner>;
  if (!acct) return <Loading />;

  return (
    <>
      <a class="muted" href="/">‹ Accounts</a>
      <h1>{formatMinor(acct.available_minor, acct.currency)}</h1>
      <div class="card">
        <div class="row"><span class="muted">IBAN</span><span class="iban">{acct.iban}</span></div>
        <div class="row"><span class="muted">Balance</span><span>{formatMinor(acct.balance_minor, acct.currency)}</span></div>
        <div class="row"><span class="muted">Status</span><span class="badge">{acct.status}</span></div>
      </div>

      <h2>Statement</h2>
      <div class="card">
        {entries.length === 0 && done && <div class="muted">No transactions yet.</div>}
        {entries.map((e) => {
          const credit = e.signed_amount >= 0;
          return (
            <div key={e.id} class="list-item">
              <div class="row">
                <span>{e.description || (credit ? "Credit" : "Debit")}</span>
                <span class={credit ? "pos" : "neg"} style="font-weight:600">
                  {credit ? "+" : "−"}{formatMinor(e.amount_minor, e.currency)}
                </span>
              </div>
              <div class="row">
                <span class="muted" style="font-size:13px">
                  {e.counterparty_owner || e.counterparty_iban || ""}
                </span>
                <span class="muted" style="font-size:13px">
                  {new Date(e.posted_at).toLocaleDateString()} · bal {formatMinor(e.balance_after, e.currency)}
                </span>
              </div>
            </div>
          );
        })}
      </div>
      {!done && (
        <button class="ghost block" onClick={loadMore} disabled={busy}>
          {busy ? "Loading…" : "Load more"}
        </button>
      )}
    </>
  );
}

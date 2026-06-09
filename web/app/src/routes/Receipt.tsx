import { useEffect, useState } from "preact/hooks";
import { api } from "../api/client";
import { formatMinor } from "../lib/money";
import type { Transfer } from "../api/types";

export function Receipt({ id }: { id: string }) {
  const [t, setT] = useState<Transfer | null>(null);
  const [err, setErr] = useState("");

  useEffect(() => {
    api.getTransfer(id).then(setT).catch((e) => setErr(e.message));
  }, [id]);

  if (err) return <div class="error">{err}</div>;
  if (!t) return <div class="center">Loading…</div>;

  const posted = t.status === "posted";
  return (
    <>
      <div class="center" style="padding:20px 0 8px">
        <div class="amount pos">{formatMinor(t.amount_minor, t.currency)}</div>
        <div class="badge" style="margin-top:8px">{t.status}</div>
      </div>
      <div class="card">
        {!posted && (
          <p class="muted">
            This transfer is <strong>{t.status}</strong>
            {t.status === "pending" ? " — awaiting settlement." : "."}
          </p>
        )}
        <div class="row"><span class="muted">Reference</span><span class="iban">{t.id.slice(0, 8)}</span></div>
        {t.description && <div class="row"><span class="muted">Note</span><span>{t.description}</span></div>}
        {t.posted_at && (
          <div class="row"><span class="muted">Posted</span><span>{new Date(t.posted_at).toLocaleString()}</span></div>
        )}
      </div>
      <a class="btn block" href="/" style="text-align:center;display:block;text-decoration:none">Back to home</a>
    </>
  );
}

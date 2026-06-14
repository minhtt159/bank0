import { useEffect, useState } from "preact/hooks";
import { api } from "../api/client";
import { disputeCategoryLabel, disputeStatusLabel } from "../lib/labels";
import type { Dispute } from "../api/types";

export function Disputes() {
  const [disputes, setDisputes] = useState<Dispute[] | null>(null);
  const [err, setErr] = useState("");

  useEffect(() => {
    api.disputes().then(setDisputes).catch((e) => setErr(e.message));
  }, []);

  if (err) return <div class="error">{err}</div>;
  if (!disputes) return <div class="center">Loading…</div>;

  return (
    <>
      <a class="muted" href="/profile">‹ Profile</a>
      <h1>My disputes</h1>
      {disputes.length === 0 && (
        <div class="center">
          You haven't reported any problems. Open a payment and tap "Report a problem"
          if something looks wrong.
        </div>
      )}
      {disputes.map((d) => (
        <div key={d.id} class="card">
          <div class="row">
            <span>{disputeCategoryLabel(d.category)}</span>
            <span class="badge">{disputeStatusLabel(d.status)}</span>
          </div>
          {d.reason && <p class="muted" style="margin:8px 0 0">{d.reason}</p>}
          {d.resolution_note && (
            <div class="row" style="margin-top:10px">
              <span class="muted">Resolution</span>
              <span style="text-align:right">{d.resolution_note}</span>
            </div>
          )}
          <div class="row" style="margin-top:10px">
            <a class="muted" href={`/transfer/${d.transfer_id}`} style="font-size:13px">
              View payment
            </a>
            {d.created_at && (
              <span class="muted" style="font-size:13px">
                {new Date(d.created_at).toLocaleDateString()}
              </span>
            )}
          </div>
        </div>
      ))}
    </>
  );
}

import { useEffect, useRef, useState } from "preact/hooks";
import { api } from "../api/client";
import { formatMinor } from "../lib/money";
import type { TransferListItem } from "../api/types";

const PAGE = 25;
type Filter = "all" | "out" | "in";

export function Activity() {
  const [items, setItems] = useState<TransferListItem[]>([]);
  const [filter, setFilter] = useState<Filter>("all");
  const [q, setQ] = useState("");
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  // A monotonically increasing token: each filter/search change bumps it so a
  // late-arriving page from a previous query can't append to the new list.
  const reqId = useRef(0);

  async function loadMore(reset: boolean) {
    if (busy) return;
    setBusy(true);
    setErr("");
    const mine = reset ? ++reqId.current : reqId.current;
    try {
      const last = reset ? undefined : items[items.length - 1];
      const page = await api.listTransfers({
        cursor: last?.requested_at,
        cursorId: last?.id,
        direction: filter === "all" ? undefined : filter,
        q,
        limit: PAGE,
      });
      if (mine !== reqId.current) return; // a newer query superseded this one
      setItems((cur) => (reset ? page : [...cur, ...page]));
      setDone(page.length < PAGE);
    } catch (e) {
      if (mine === reqId.current) setErr((e as Error).message);
    } finally {
      if (mine === reqId.current) setBusy(false);
    }
  }

  // Reload from scratch whenever the filter or (debounced) search changes.
  useEffect(() => {
    const t = setTimeout(() => loadMore(true), 200);
    return () => clearTimeout(t);
  }, [filter, q]);

  return (
    <>
      <a class="muted" href="/">‹ Home</a>
      <h1>Activity</h1>

      <input placeholder="Search payments" value={q}
        onInput={(e) => setQ((e.target as HTMLInputElement).value)} />
      <div class="row" style="gap:8px;margin:10px 0">
        {(["all", "out", "in"] as Filter[]).map((f) => (
          <button key={f} class={filter === f ? "" : "ghost"} style="flex:1;padding:8px"
            onClick={() => setFilter(f)}>
            {f === "all" ? "All" : f === "out" ? "Sent" : "Received"}
          </button>
        ))}
      </div>

      {err && <div class="error">{err}</div>}

      <div class="card">
        {items.length === 0 && done && !busy && <div class="muted">No payments found.</div>}
        {items.map((t) => {
          const credit = t.direction === "in";
          return (
            <a key={t.id} class="list-item card tappable" href={`/transfer/${t.id}`}
              style="border:0;border-bottom:1px solid var(--line);border-radius:0;margin:0;padding:12px 0">
              <div class="row">
                <span>{t.description || t.counterparty_owner || (credit ? "Received" : "Sent")}</span>
                <span class={credit ? "pos" : "neg"} style="font-weight:600">
                  {credit ? "+" : "−"}{formatMinor(t.amount_minor, t.currency)}
                </span>
              </div>
              <div class="row">
                <span class="muted" style="font-size:13px">
                  {t.counterparty_iban || t.counterparty_owner || ""}
                </span>
                <span class="muted" style="font-size:13px">
                  {t.requested_at ? new Date(t.requested_at).toLocaleDateString() : ""}
                  {t.status !== "posted" ? ` · ${t.status}` : ""}
                </span>
              </div>
            </a>
          );
        })}
      </div>

      {!done && items.length > 0 && (
        <button class="ghost block" onClick={() => loadMore(false)} disabled={busy}>
          {busy ? "Loading…" : "Load more"}
        </button>
      )}
    </>
  );
}

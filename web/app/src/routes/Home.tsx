import { useEffect, useState } from "preact/hooks";
import { api } from "../api/client";
import { userId } from "../store/auth";
import { formatMinor } from "../lib/money";
import type { Account } from "../api/types";

export function Home() {
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  const [err, setErr] = useState("");

  useEffect(() => {
    api.accounts(userId.value).then(setAccounts).catch((e) => setErr(e.message));
  }, []);

  if (err) return <div class="error">{err}</div>;
  if (!accounts) return <div class="center">Loading…</div>;
  if (accounts.length === 0) return <div class="center">You have no accounts yet.</div>;

  return (
    <>
      <h1>Your accounts</h1>
      {accounts.map((a) => (
        <a key={a.id} class="card tappable" href={`/accounts/${a.id}`}>
          <div class="row">
            <span class="muted">{a.kind}{a.is_default ? " · default" : ""}</span>
            {a.status !== "active" && <span class="badge">{a.status}</span>}
          </div>
          <div class="amount" style="margin:6px 0">{formatMinor(a.available_minor, a.currency)}</div>
          <div class="row">
            <span class="iban muted">{a.iban}</span>
            {a.available_minor !== a.balance_minor && (
              <span class="muted">balance {formatMinor(a.balance_minor, a.currency)}</span>
            )}
          </div>
        </a>
      ))}
    </>
  );
}

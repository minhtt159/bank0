import { useEffect, useState } from "preact/hooks";
import { api, ApiError } from "../api/client";
import type { Invitation } from "../api/types";
import { ErrorBanner, Loading } from "../lib/feedback";

const STATUS_LABEL: Record<Invitation["status"], string> = {
  pending: "Pending",
  consumed: "Used",
  expired: "Expired",
};

function generateError(e: unknown): string {
  if (!(e instanceof ApiError)) return "Could not create an invitation. Please try again.";
  switch (e.status) {
    case 403: return "Verify your account first to invite others.";
    case 409: return "You've used all your invitations.";
    default: return e.message || "Could not create an invitation. Please try again.";
  }
}

export function Invite() {
  const [remaining, setRemaining] = useState<number | null>(null);
  const [invites, setInvites] = useState<Invitation[] | null>(null);
  const [loadErr, setLoadErr] = useState("");

  const [minted, setMinted] = useState("");   // most-recently created code
  const [genErr, setGenErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [copyMsg, setCopyMsg] = useState(""); // announced via the live region

  useEffect(() => {
    api.me().then((m) => setRemaining(m.invites_remaining ?? 0)).catch((e) => setLoadErr(e.message));
    api.listInvitations().then(setInvites).catch((e) => setLoadErr(e.message));
  }, []);

  async function generate() {
    setBusy(true);
    setGenErr("");
    setCopyMsg("");
    try {
      const r = await api.createInvitation();
      setMinted(r.code);
      setRemaining(r.invites_remaining);
      // Prepend so the freshest invitation leads the list (server orders newest first).
      setInvites((cur) => [
        { code: r.code, status: "pending", created_at: new Date().toISOString(), expires_at: r.expires_at },
        ...(cur ?? []),
      ]);
    } catch (e) {
      setGenErr(generateError(e));
    } finally {
      setBusy(false);
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(minted);
      setCopyMsg("Invitation code copied to clipboard.");
    } catch {
      setCopyMsg("Couldn't copy automatically — select the code and copy it manually.");
    }
  }

  if (loadErr && !invites) return <ErrorBanner>{loadErr}</ErrorBanner>;
  if (remaining == null || !invites) return <Loading />;

  const canGenerate = remaining > 0 && !busy;

  return (
    <>
      <a class="muted" href="/profile">‹ Profile</a>
      <h1>Invite a friend</h1>
      <p class="muted">Share a single-use code to let someone open a bank0 account.</p>

      <div class="card">
        <div class="row">
          <span class="muted">Invitations remaining</span>
          <span aria-live="polite">{remaining}</span>
        </div>
      </div>

      {genErr && <ErrorBanner>{genErr}</ErrorBanner>}
      <button class="block" onClick={generate} disabled={!canGenerate}>
        {busy ? "Generating…" : "Generate invitation"}
      </button>
      {remaining === 0 && !genErr && (
        <p class="muted" style="margin-top:8px">You've used all your invitations.</p>
      )}

      {minted && (
        <div class="card" style="margin-top:16px;border-color:var(--accent)">
          <div class="muted" style="font-size:13px">New invitation code</div>
          <div class="amount iban" style="margin:6px 0 12px;word-break:break-all">{minted}</div>
          <button class="ghost block" onClick={copy}>Copy code</button>
        </div>
      )}
      {/* Live region: announces the copy result to screen readers (not colour-only). */}
      <p class="muted" role="status" aria-live="polite" style={copyMsg ? "margin-top:8px" : "position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0 0 0 0)"}>
        {copyMsg}
      </p>

      <h2 style="margin-top:24px">Your invitations</h2>
      {invites.length === 0 && <div class="center">You haven't created any invitations yet.</div>}
      {invites.map((inv) => (
        <div key={inv.code} class="card">
          <div class="row">
            <span class="iban">{inv.code}</span>
            <span class="badge">{STATUS_LABEL[inv.status]}</span>
          </div>
          <div class="muted" style="font-size:13px;margin-top:6px">
            {inv.status === "consumed" && inv.consumed_at
              ? `Used ${new Date(inv.consumed_at).toLocaleDateString()}`
              : `Expires ${new Date(inv.expires_at).toLocaleDateString()}`}
          </div>
        </div>
      ))}
    </>
  );
}

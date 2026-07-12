import { useEffect, useState } from "preact/hooks";
import { api, ApiError } from "../api/client";
import type { Session } from "../api/types";
import { ErrorBanner, Loading } from "../lib/feedback";

export function Devices() {
  const [sessions, setSessions] = useState<Session[] | null>(null);
  const [err, setErr] = useState("");
  const [revoking, setRevoking] = useState("");

  function load() {
    api.sessions().then(setSessions).catch((e) => setErr(e.message));
  }
  useEffect(load, []);

  async function revoke(familyId: string) {
    setErr("");
    setRevoking(familyId);
    try {
      await api.revokeSession(familyId);
      setSessions((cur) => (cur ?? []).filter((s) => s.family_id !== familyId));
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Could not revoke session");
    } finally {
      setRevoking("");
    }
  }

  if (err && !sessions) return <ErrorBanner>{err}</ErrorBanner>;
  if (!sessions) return <Loading />;

  return (
    <>
      <a class="muted" href="/profile">‹ Profile</a>
      <h1>Devices &amp; sessions</h1>
      <p class="muted">Active sign-ins on your account. Revoke any you don't recognise.</p>
      {err && <ErrorBanner>{err}</ErrorBanner>}
      {sessions.length === 0 && <div class="center">No active sessions.</div>}
      {sessions.map((s) => (
        <div key={s.family_id} class="card">
          <div class="row">
            <span>{s.device_label || "Device"}</span>
            {s.current && <span class="badge">this device</span>}
          </div>
          {s.user_agent && (
            <div class="muted" style="font-size:13px;margin-top:4px;word-break:break-word">{s.user_agent}</div>
          )}
          <div class="row" style="margin-top:8px">
            <span class="muted" style="font-size:13px">
              {s.ip || ""}
              {s.last_seen_at ? ` · last seen ${new Date(s.last_seen_at).toLocaleString()}` : ""}
            </span>
          </div>
          {!s.current && (
            <button class="ghost block" style="margin-top:10px"
              onClick={() => revoke(s.family_id)} disabled={revoking === s.family_id}>
              {revoking === s.family_id ? "Revoking…" : "Revoke"}
            </button>
          )}
        </div>
      ))}
    </>
  );
}

import { useState } from "preact/hooks";
import { useLocation } from "preact-iso";
import { api, ApiError } from "../api/client";
import { setAuth, isAuthed } from "../store/auth";
import { takeNotice } from "../lib/onboarding";
import { ErrorBanner } from "../lib/feedback";

export function Login() {
  const { route } = useLocation();
  if (isAuthed.value) {
    route("/", true);
    return null;
  }
  const [username, setU] = useState("");
  const [password, setP] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  // One-shot success notice handed over from the verify flow (consumed on read).
  const [notice] = useState(() => takeNotice());

  async function submit(e: Event) {
    e.preventDefault();
    setBusy(true);
    setErr("");
    try {
      const r = await api.login(username, password);
      setAuth({ token: r.token, userId: r.user_id, expiresAt: r.expires_at, refreshToken: r.refresh_token });
      route("/", true);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Sign in failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div class="login-wrap">
      <div class="logo">bank0</div>
      <form onSubmit={submit}>
        {notice && (
          <p class="muted center" role="status" aria-live="polite" style="padding:0 0 4px">{notice}</p>
        )}
        {err && <ErrorBanner>{err}</ErrorBanner>}
        <label for="u">Username</label>
        <input id="u" autocomplete="username" value={username}
          onInput={(e) => setU((e.target as HTMLInputElement).value)} />
        <label for="p">Password</label>
        <input id="p" type="password" autocomplete="current-password" value={password}
          onInput={(e) => setP((e.target as HTMLInputElement).value)} />
        <button class="block" style="margin-top:20px" disabled={busy || !username || !password}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
      <p class="center" style="padding:20px 0 0">
        <a class="muted" href="/register">Create account</a>
      </p>
    </div>
  );
}

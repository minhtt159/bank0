import { useState } from "preact/hooks";
import { useLocation } from "preact-iso";
import { api, ApiError } from "../api/client";
import { clearAuth } from "../store/auth";
import { stashNotice } from "../lib/onboarding";
import { ErrorBanner } from "../lib/feedback";

const MIN_LEN = 12; // matches ChangePasswordRequest.new_password minLength in the spec

export function ChangePassword() {
  const { route } = useLocation();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  const tooShort = next.length > 0 && next.length < MIN_LEN;
  const mismatch = confirm.length > 0 && next !== confirm;
  const canSubmit = !!current && next.length >= MIN_LEN && next === confirm && !busy;

  async function submit(e: Event) {
    e.preventDefault();
    if (!canSubmit) return;
    setBusy(true);
    setErr("");
    try {
      await api.changePassword(current, next);
      // Every session went with the old password, this one included, so there is
      // nothing left to render behind Protected — hand the message to /login.
      stashNotice("Password changed. Every device was signed out — sign in with your new password.");
      clearAuth();
      route("/login", true);
      return;
      setCurrent("");
      setNext("");
      setConfirm("");
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Could not change password");
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <a class="muted" href="/profile">‹ Profile</a>
      <h1>Change password</h1>
      <form onSubmit={submit}>
        {err && <ErrorBanner>{err}</ErrorBanner>}
        <label for="cur">Current password</label>
        <input id="cur" type="password" autocomplete="current-password" value={current}
          onInput={(e) => setCurrent((e.target as HTMLInputElement).value)} />
        <label for="new">New password</label>
        <input id="new" type="password" autocomplete="new-password" value={next}
          onInput={(e) => setNext((e.target as HTMLInputElement).value)} />
        {tooShort && <ErrorBanner small>Use at least {MIN_LEN} characters.</ErrorBanner>}
        <label for="conf">Confirm new password</label>
        <input id="conf" type="password" autocomplete="new-password" value={confirm}
          onInput={(e) => setConfirm((e.target as HTMLInputElement).value)} />
        {mismatch && <ErrorBanner small>Passwords don't match.</ErrorBanner>}
        <button class="block" style="margin-top:20px" disabled={!canSubmit}>
          {busy ? "Saving…" : "Change password"}
        </button>
      </form>
    </>
  );
}

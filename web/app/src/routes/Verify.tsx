import { useState } from "preact/hooks";
import { useLocation } from "preact-iso";
import { api, ApiError } from "../api/client";
import { readPending, clearPending, stashNotice } from "../lib/onboarding";
import { ErrorBanner } from "../lib/feedback";

function verifyError(e: unknown): string {
  if (!(e instanceof ApiError)) return "Could not verify that code. Please try again.";
  switch (e.status) {
    case 401: return "That code is incorrect. Check it and try again.";
    case 404: return "This verification session has expired. Please register again.";
    case 422: return "Too many attempts. Request a new code to continue.";
    case 429: return "Too many attempts — please wait a moment and try again.";
    default: return e.message || "Could not verify that code. Please try again.";
  }
}

export function Verify() {
  const { route } = useLocation();
  // Capture the handoff once. Absent (deep-link / reload after tab close) -> register.
  const [pending] = useState(() => readPending());
  if (!pending) {
    route("/register", true);
    return null;
  }

  const [code, setCode] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [resendMsg, setResendMsg] = useState("");
  const [resending, setResending] = useState(false);

  const where = pending.channel === "phone" ? "your phone" : "your email";

  async function submit(e: Event) {
    e.preventDefault();
    if (!code.trim() || busy) return;
    setBusy(true);
    setErr("");
    try {
      const r = await api.verifyContact({ verify_token: pending!.verify_token, code: code.trim() });
      if (r.login_ready) {
        clearPending();
        stashNotice("Your account is verified — please sign in.");
        route("/login", true);
        return;
      }
      // Verified a channel but not yet login-ready (shouldn't happen in the
      // single-channel flow) — keep the user here with a gentle nudge.
      setErr("Contact verified, but your account isn't ready to sign in yet.");
    } catch (e) {
      setErr(verifyError(e));
    } finally {
      setBusy(false);
    }
  }

  async function resend() {
    if (resending) return;
    setResending(true);
    setResendMsg("");
    setErr("");
    try {
      await api.resendCode({ verify_token: pending!.verify_token });
      setResendMsg(`If a code is pending, a new one is on its way to ${where}.`);
    } catch (e) {
      // 202 is the norm; the only error that reaches here is the cooldown.
      setResendMsg(
        e instanceof ApiError && e.status === 429
          ? "Please wait a little longer before requesting another code."
          : "Could not resend the code just now. Please try again shortly.",
      );
    } finally {
      setResending(false);
    }
  }

  return (
    <div class="login-wrap">
      <div class="logo">bank0</div>
      <h1 style="text-align:center">Verify your contact</h1>
      <p class="muted center" style="padding:0 0 8px">
        Enter the 6-digit code we sent to {where}.
      </p>
      <form onSubmit={submit}>
        {err && <ErrorBanner>{err}</ErrorBanner>}
        <label for="code">Verification code</label>
        <input id="code" inputMode="numeric" autocomplete="one-time-code"
          autoFocus value={code}
          onInput={(e) => setCode((e.target as HTMLInputElement).value)} />
        <button class="block" style="margin-top:20px" disabled={!code.trim() || busy}>
          {busy ? "Verifying…" : "Verify"}
        </button>
      </form>
      <button class="ghost block" style="margin-top:12px" onClick={resend} disabled={resending}>
        {resending ? "Sending…" : "Resend code"}
      </button>
      {resendMsg && (
        <p class="muted center" role="status" aria-live="polite" style="padding:12px 0 0">
          {resendMsg}
        </p>
      )}
    </div>
  );
}

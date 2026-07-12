import { useState } from "preact/hooks";
import { useLocation } from "preact-iso";
import { api, ApiError } from "../api/client";
import { isAuthed } from "../store/auth";
import { stashPending } from "../lib/onboarding";
import { ErrorBanner } from "../lib/feedback";

const MIN_LEN = 12; // matches RegisterRequest.password minLength in the spec

// Translate the register SQLSTATE-mapped HTTP status into friendly inline copy.
// Falls back to the server's message for 422 (field-level validation detail).
function registerError(e: unknown): string {
  if (!(e instanceof ApiError)) return "Could not create your account. Please try again.";
  switch (e.status) {
    case 400: return "Something went wrong preparing your request — please try again.";
    case 404: return "That invitation code was not found. Check it and try again.";
    case 409: return "That invitation code has already been used or has expired.";
    case 422: return e.message || "Please check the details you entered.";
    case 429: return "Too many attempts — please slow down and try again shortly.";
    default: return e.message || "Could not create your account. Please try again.";
  }
}

export function Register() {
  const { route } = useLocation();
  if (isAuthed.value) {
    route("/", true);
    return null;
  }

  const [username, setU] = useState("");
  const [fullName, setFullName] = useState("");
  const [email, setEmail] = useState("");
  const [phone, setPhone] = useState("");
  const [password, setP] = useState("");
  const [code, setCode] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  // One Idempotency-Key per form session (copy Transfer's pattern): the lazy
  // initializer mints it once, so every retry of the same submission replays
  // rather than creating a second pending-verification user.
  const [idemKey] = useState(() => crypto.randomUUID());

  const tooShort = password.length > 0 && password.length < MIN_LEN;
  const hasContact = !!email.trim() || !!phone.trim();
  const canSubmit =
    !!username.trim() && !!fullName.trim() && password.length >= MIN_LEN &&
    hasContact && !!code.trim() && !busy;

  async function submit(e: Event) {
    e.preventDefault();
    if (!canSubmit) return;
    setBusy(true);
    setErr("");
    try {
      const r = await api.register(
        {
          username: username.trim(),
          password,
          full_name: fullName.trim(),
          email: email.trim() || undefined,
          phone_number: phone.trim() || undefined,
          invitation_code: code.trim(),
        },
        idemKey,
      );
      // Hand the opaque verify_token to /verify via sessionStorage (never the URL).
      stashPending({ verify_token: r.verify_token, channel: r.verify_channel });
      route("/verify", true);
    } catch (e) {
      setErr(registerError(e));
      setBusy(false); // keep idemKey so a corrected resubmit dedupes on the server
    }
  }

  return (
    <div class="login-wrap">
      <div class="logo">bank0</div>
      <form onSubmit={submit}>
        {err && <ErrorBanner>{err}</ErrorBanner>}

        <label for="u">Username</label>
        <input id="u" autocomplete="username" value={username}
          onInput={(e) => setU((e.target as HTMLInputElement).value)} />

        <label for="fn">Full name</label>
        <input id="fn" autocomplete="name" value={fullName}
          onInput={(e) => setFullName((e.target as HTMLInputElement).value)} />

        <label for="em">Email</label>
        <input id="em" type="email" autocomplete="email" value={email}
          onInput={(e) => setEmail((e.target as HTMLInputElement).value)} />

        <label for="ph">Phone number</label>
        <input id="ph" type="tel" autocomplete="tel" value={phone}
          onInput={(e) => setPhone((e.target as HTMLInputElement).value)} />
        {!hasContact && (username || fullName) && (
          <ErrorBanner small>Enter an email address or a phone number so we can verify you.</ErrorBanner>
        )}

        <label for="p">Password</label>
        <input id="p" type="password" autocomplete="new-password" value={password}
          onInput={(e) => setP((e.target as HTMLInputElement).value)} />
        {tooShort && <ErrorBanner small>Use at least {MIN_LEN} characters.</ErrorBanner>}

        <label for="code">Invitation code</label>
        <input id="code" value={code} autocomplete="off"
          onInput={(e) => setCode((e.target as HTMLInputElement).value)} />

        <button class="block" style="margin-top:20px" disabled={!canSubmit}>
          {busy ? "Creating account…" : "Create account"}
        </button>
      </form>
      <p class="center" style="padding:20px 0 0">
        <a class="muted" href="/login">Already have an account? Sign in</a>
      </p>
    </div>
  );
}

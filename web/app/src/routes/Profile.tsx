import { useEffect, useState } from "preact/hooks";
import { api, ApiError } from "../api/client";
import type { User } from "../api/types";

export function Profile() {
  const [me, setMe] = useState<User | null>(null);
  const [err, setErr] = useState("");

  // edit state
  const [editing, setEditing] = useState(false);
  const [fullName, setFullName] = useState("");
  const [email, setEmail] = useState("");
  const [phone, setPhone] = useState("");
  const [saveErr, setSaveErr] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.me().then(setMe).catch((e) => setErr(e.message));
  }, []);

  function startEdit() {
    if (!me) return;
    setFullName(me.full_name);
    setEmail(me.email ?? "");
    setPhone(me.phone_number ?? "");
    setSaveErr("");
    setEditing(true);
  }

  async function save(e: Event) {
    e.preventDefault();
    setBusy(true);
    setSaveErr("");
    try {
      // Partial PATCH: send only the fields the customer can change. Empty email/phone
      // are sent as empty strings to clear them; full_name must stay non-empty.
      const updated = await api.updateMe({
        full_name: fullName.trim(),
        email: email.trim(),
        phone_number: phone.trim(),
      });
      setMe(updated);
      setEditing(false);
    } catch (e) {
      setSaveErr(e instanceof ApiError ? e.message : "Could not save changes");
    } finally {
      setBusy(false);
    }
  }

  if (err) return <div class="error">{err}</div>;
  if (!me) return <div class="center">Loading…</div>;

  if (editing) {
    return (
      <>
        <a class="muted" href="#" onClick={(e) => { e.preventDefault(); setEditing(false); }}>‹ Cancel</a>
        <h1>Edit details</h1>
        <form onSubmit={save}>
          {saveErr && <div class="error">{saveErr}</div>}
          <label for="fn">Full name</label>
          <input id="fn" value={fullName}
            onInput={(e) => setFullName((e.target as HTMLInputElement).value)} />
          <label for="em">Email</label>
          <input id="em" type="email" autocomplete="email" value={email}
            onInput={(e) => setEmail((e.target as HTMLInputElement).value)} />
          <label for="ph">Phone number</label>
          <input id="ph" type="tel" autocomplete="tel" value={phone}
            onInput={(e) => setPhone((e.target as HTMLInputElement).value)} />
          <button class="block" style="margin-top:20px" disabled={busy || !fullName.trim()}>
            {busy ? "Saving…" : "Save changes"}
          </button>
        </form>
      </>
    );
  }

  return (
    <>
      <h1>Your details</h1>
      <div class="card">
        <div class="row"><span class="muted">Name</span><span>{me.full_name}</span></div>
        <div class="row"><span class="muted">Username</span><span>{me.username}</span></div>
        <div class="row"><span class="muted">Email</span><span>{me.email || "—"}</span></div>
        <div class="row"><span class="muted">Phone</span><span>{me.phone_number || "—"}</span></div>
        <div class="row"><span class="muted">Status</span><span class="badge">{me.status}</span></div>
      </div>
      <button class="ghost block" onClick={startEdit}>Edit details</button>

      <h2 style="margin-top:24px">Activity &amp; security</h2>
      <a class="card tappable" href="/activity">
        <div class="row"><span>Activity</span><span class="muted">›</span></div>
      </a>
      <a class="card tappable" href="/disputes">
        <div class="row"><span>My disputes</span><span class="muted">›</span></div>
      </a>
      <a class="card tappable" href="/devices">
        <div class="row"><span>Devices &amp; sessions</span><span class="muted">›</span></div>
      </a>
      <a class="card tappable" href="/password">
        <div class="row"><span>Change password</span><span class="muted">›</span></div>
      </a>
    </>
  );
}

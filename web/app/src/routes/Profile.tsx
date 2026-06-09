import { useEffect, useState } from "preact/hooks";
import { api } from "../api/client";
import type { User } from "../api/types";

export function Profile() {
  const [me, setMe] = useState<User | null>(null);
  const [err, setErr] = useState("");

  useEffect(() => {
    api.me().then(setMe).catch((e) => setErr(e.message));
  }, []);

  if (err) return <div class="error">{err}</div>;
  if (!me) return <div class="center">Loading…</div>;

  return (
    <>
      <h1>Your details</h1>
      <div class="card">
        <div class="row"><span class="muted">Name</span><span>{me.full_name}</span></div>
        <div class="row"><span class="muted">Username</span><span>{me.username}</span></div>
        {me.email && <div class="row"><span class="muted">Email</span><span>{me.email}</span></div>}
        {me.phone_number && <div class="row"><span class="muted">Phone</span><span>{me.phone_number}</span></div>}
        <div class="row"><span class="muted">Status</span><span class="badge">{me.status}</span></div>
      </div>
    </>
  );
}

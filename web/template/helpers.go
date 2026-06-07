package template

import (
	"strconv"

	"github.com/google/uuid"
)

func i64(n int64) string { return strconv.FormatInt(n, 10) }
func itoa(n int) string  { return strconv.Itoa(n) }

// derefI64 reads an optional signed amount (nil -> 0).
func derefI64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// deref renders an optional string (nil -> "").
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// orDash returns the first non-empty of a, b, else an em dash.
func orDash(a *string, b *string) string {
	if a != nil && *a != "" {
		return *a
	}
	if b != nil && *b != "" {
		return *b
	}
	return "—"
}

// uuidStr renders an optional uuid (nil -> "").
func uuidStr(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// newKey mints a fresh idempotency key when a money form is rendered, so a
// double-submit of the same form replays rather than duplicating (docs/04 §5.2).
func newKey() string { return uuid.NewString() }

// shortID renders the first 8 chars of an optional uuid (nil -> "—").
func shortID(id *uuid.UUID) string {
	if id == nil {
		return "—"
	}
	return id.String()[:8]
}

// styleTag is rendered via templ.Raw so the CSS braces aren't parsed as templ
// expressions.
const styleTag = `<style>
  :root { --bg:#0f1419; --panel:#1a2230; --ink:#e6edf3; --muted:#8b98a5; --ok:#1f9d55; --bad:#e5484d; --accent:#3b82f6; --line:#2a3441; }
  * { box-sizing:border-box; }
  body { margin:0; font:14px/1.5 system-ui,sans-serif; background:var(--bg); color:var(--ink); }
  .topbar { display:flex; gap:.6rem; align-items:baseline; padding:.8rem 1.2rem; background:var(--panel); border-bottom:1px solid var(--line); }
  .topbar strong { font-size:1.1rem; } .topbar span { color:var(--muted); }
  main { padding:1.2rem; max-width:1100px; margin:0 auto; }
  h1 { font-size:1.3rem; margin:.2rem 0 1rem; }
  .badge { padding:.7rem 1rem; border-radius:8px; font-weight:600; margin-bottom:1rem; }
  .badge.ok { background:rgba(31,157,85,.15); color:#3ddc84; border:1px solid var(--ok); }
  .badge.bad { background:rgba(229,72,77,.15); color:#ff7b7b; border:1px solid var(--bad); }
  .cards { display:grid; grid-template-columns:repeat(auto-fit,minmax(180px,1fr)); gap:.8rem; margin-bottom:1.2rem; }
  .card { background:var(--panel); border:1px solid var(--line); border-radius:10px; padding:1rem; }
  .card span { color:var(--muted); display:block; font-size:.8rem; text-transform:uppercase; letter-spacing:.04em; }
  .card strong { font-size:1.4rem; }
  .tabs { display:flex; gap:.5rem; margin-bottom:1rem; }
  .tabs button { background:var(--panel); color:var(--ink); border:1px solid var(--line); padding:.5rem .9rem; border-radius:8px; cursor:pointer; }
  .tabs button:hover { border-color:var(--accent); }
  table { width:100%; border-collapse:collapse; background:var(--panel); border:1px solid var(--line); border-radius:10px; overflow:hidden; }
  th,td { text-align:left; padding:.6rem .8rem; border-bottom:1px solid var(--line); }
  th { color:var(--muted); font-size:.78rem; text-transform:uppercase; letter-spacing:.04em; }
  tr:last-child td { border-bottom:none; }
  .num { text-align:right; font-variant-numeric:tabular-nums; }
  .pill { padding:.1rem .5rem; border-radius:999px; font-size:.75rem; border:1px solid var(--line); }
  .muted { color:var(--muted); }
  .spacer { flex:1; }
  .linkbtn { background:none; border:none; color:var(--accent); cursor:pointer; font:inherit; padding:0; }
  .linkbtn:hover { text-decoration:underline; }
  .login { max-width:340px; margin:3rem auto; background:var(--panel); border:1px solid var(--line); border-radius:12px; padding:1.6rem; }
  .login h1 { margin:0 0 .2rem; }
  .login label { display:block; margin:.9rem 0 .2rem; color:var(--muted); font-size:.85rem; }
  .login input { width:100%; padding:.55rem .7rem; border-radius:8px; border:1px solid var(--line); background:var(--bg); color:var(--ink); }
  button.primary { margin-top:1.2rem; width:100%; background:var(--accent); color:#fff; border:none; padding:.6rem; border-radius:8px; cursor:pointer; font-weight:600; }
  button.primary:hover { filter:brightness(1.1); }
  .actions { white-space:nowrap; }
  .btn { border:1px solid var(--line); background:var(--bg); color:var(--ink); padding:.3rem .7rem; border-radius:6px; cursor:pointer; font-size:.82rem; margin-right:.3rem; }
  .btn.ok { border-color:var(--ok); color:#3ddc84; }
  .btn.ok:hover { background:rgba(31,157,85,.15); }
  .btn.danger { border-color:var(--bad); color:#ff7b7b; }
  .btn.danger:hover { background:rgba(229,72,77,.15); }
  .flash { background:rgba(59,130,246,.15); border:1px solid var(--accent); color:#9ec5ff; padding:.5rem .8rem; border-radius:8px; margin-bottom:.8rem; }
  /* app layout (docs/04 §3 IA) */
  /* proportional 3-column layout (~14% / 57% / 29%); minmax(0,..) keeps wide
     content from forcing the grid past the viewport. */
  .layout { display:grid; grid-template-columns:minmax(0,1fr) minmax(0,4fr) minmax(0,2fr); min-height:calc(100vh - 49px); }
  @media (max-width:900px) { .layout { grid-template-columns:minmax(0,1fr) minmax(0,3fr) minmax(0,3fr); } }
  .leftnav { border-right:1px solid var(--line); padding:.8rem .5rem; display:flex; flex-direction:column; gap:.15rem; background:var(--panel); min-width:0; }
  .navitem { padding:.5rem .7rem; border-radius:8px; color:var(--ink); cursor:pointer; text-decoration:none; font-size:.9rem; }
  .navitem:hover { background:rgba(59,130,246,.12); }
  .navitem.disabled { color:var(--muted); cursor:default; }
  .badge-count { background:var(--bad); color:#fff; border-radius:999px; padding:0 .4rem; font-size:.72rem; margin-left:.35rem; }
  .navitem.disabled:hover { background:none; }
  #main-panel { padding:1.2rem 1.4rem; overflow:auto; min-width:0; }
  #rail { border-left:1px solid var(--line); padding:1rem 1.1rem; background:#141b27; overflow:auto; min-width:0; overflow-wrap:anywhere; }
  .rail-empty { margin-top:2rem; text-align:center; }
  .panel { min-width:0; }
  .panel-head { display:flex; align-items:center; gap:.8rem; margin-bottom:1rem; }
  .panel-head h1 { font-size:1.2rem; margin:0; white-space:nowrap; }
  .search { flex:1; min-width:0; max-width:460px; padding:.45rem .7rem; border-radius:8px; border:1px solid var(--line); background:var(--bg); color:var(--ink); }
  .search:focus { outline:none; border-color:var(--accent); }
  .small { font-size:.8rem; }
  .mono { font-family:ui-monospace,monospace; font-size:.8rem; }
  .link { color:var(--accent); cursor:pointer; text-decoration:none; }
  .link:hover { text-decoration:underline; }
  .link.sm { margin-left:auto; font-size:.8rem; }
  .kv { display:flex; gap:.8rem; padding:.3rem 0; border-bottom:1px solid var(--line); font-size:.88rem; }
  .kv > span:first-child { color:var(--muted); min-width:96px; }
  .loadmore-row td { text-align:center; padding:.6rem; }
  .btn.loadmore { width:100%; max-width:240px; }
  /* transfer-status pills */
  .pill.posted { color:#3ddc84; border-color:var(--ok); }
  .pill.pending { color:#ffd479; border-color:#ffd479; }
  .pill.failed, .pill.canceled { color:#ff7b7b; border-color:var(--bad); }
  .pill.reversed { color:var(--muted); }
  .detail { min-width:0; }
  .detail-head { border-bottom:1px solid var(--line); padding-bottom:.6rem; margin-bottom:.8rem; }
  .detail-head h2 { margin:0; font-size:1.1rem; }
  .block { margin-bottom:1.4rem; }
  .block h3 { font-size:.78rem; text-transform:uppercase; letter-spacing:.04em; color:var(--muted); margin:.2rem 0 .6rem; }
  form label { display:block; margin:.5rem 0 .15rem; font-size:.8rem; color:var(--muted); }
  form input, form select { width:100%; padding:.45rem .6rem; border-radius:7px; border:1px solid var(--line); background:var(--bg); color:var(--ink); }
  form input:disabled, form select:disabled { opacity:.55; }
  .rowlink { cursor:pointer; }
  .rowlink:hover { background:rgba(59,130,246,.10); }
  .rolebadge { padding:.1rem .55rem; border-radius:999px; font-size:.72rem; text-transform:uppercase; letter-spacing:.04em; border:1px solid var(--line); }
  .rolebadge.admin { color:#ffd479; border-color:#ffd479; }
  .rolebadge.operator { color:#9ec5ff; border-color:var(--accent); }
  .rolebadge.auditor { color:var(--muted); }
  button.primary.sm { width:auto; margin-top:0; padding:.4rem .8rem; font-size:.85rem; }
  .pill.active { color:#3ddc84; border-color:var(--ok); }
  .pill.frozen, .pill.locked { color:#ffd479; border-color:#ffd479; }
  .pill.closed { color:#ff7b7b; border-color:var(--bad); }
  .acct { border:1px solid var(--line); border-radius:10px; padding:.7rem .8rem; margin-bottom:.7rem; background:var(--bg); }
  .acct-head { display:flex; align-items:center; gap:.5rem; }
  .acct-head .iban { font-family:ui-monospace,monospace; font-size:.84rem; }
  .acct-head .star { color:#ffd479; }
  .acct-head .pill { margin-left:auto; }
  .acct-bal { display:flex; gap:1rem; flex-wrap:wrap; margin:.5rem 0; font-size:.85rem; }
  .acct-actions { border-top:1px dashed var(--line); padding-top:.5rem; }
  .inline { display:flex; gap:.4rem; align-items:center; margin-bottom:.4rem; }
  .inline input { width:auto; flex:1; }
  .acct-controls { display:flex; gap:.4rem; flex-wrap:wrap; align-items:center; }
  .acct-controls .inline { margin-bottom:0; }
  .newacct summary { cursor:pointer; color:var(--accent); font-size:.85rem; margin-top:.4rem; }
</style>`

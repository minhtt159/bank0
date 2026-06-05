package template

import "strconv"

func i64(n int64) string { return strconv.FormatInt(n, 10) }

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
</style>`

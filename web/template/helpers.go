package template

import (
	"encoding/json"
	"strconv"

	"github.com/google/uuid"
)

// jsonStr reads a top-level string field out of a JSONB column (admin_actions.detail
// etc.). Non-object bodies, missing keys, or non-string values render as "".
func jsonStr(raw []byte, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func i64(n int64) string { return strconv.FormatInt(n, 10) }

// txt renders an `any` column as a string. sqlc emits interface{} for some
// COALESCE(...::text, '') projections (e.g. disputes queue `raised_by`).
func txt(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

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
// double-submit of the same form replays rather than duplicating (docs/05 §5.2).
func newKey() string { return uuid.NewString() }

// shortID renders the first 8 chars of an optional uuid (nil -> "—").
func shortID(id *uuid.UUID) string {
	if id == nil {
		return "—"
	}
	return id.String()[:8]
}

// themeScript runs in <head> before first paint: it applies the persisted (or
// OS-preferred) theme to <html data-theme=…> so there is no flash of the wrong
// theme, and exposes toggleTheme() for the topbar button.
const themeScript = `<script>
(function () {
  var t;
  try { t = localStorage.getItem('bank0-theme'); } catch (e) {}
  if (t !== 'light' && t !== 'dark') {
    t = (window.matchMedia && matchMedia('(prefers-color-scheme: light)').matches) ? 'light' : 'dark';
  }
  document.documentElement.dataset.theme = t;
  window.toggleTheme = function () {
    var n = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
    document.documentElement.dataset.theme = n;
    try { localStorage.setItem('bank0-theme', n); } catch (e) {}
  };
})();
</script>`

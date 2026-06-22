import type { ComponentChildren } from "preact";

// Shared, accessible status surfaces for the PWA.
//
// ErrorBanner / Loading exist so error and loading states are announced to
// assistive tech in ONE place — a bare <div class="error"> is invisible to a
// screen reader. They also dedupe the banner markup repeated across every route.

// ErrorBanner: role="alert" is an implicit aria-live="assertive" region, so the
// message is announced the moment it renders. `small` matches the inline-hint size.
export function ErrorBanner({ children, small }: { children: ComponentChildren; small?: boolean }) {
  return (
    <div class="error" role="alert" style={small ? "font-size:13px" : undefined}>
      {children}
    </div>
  );
}

// Loading: a polite live region so a screen reader announces the wait without
// cutting off whatever it is currently reading.
export function Loading({ label = "Loading…" }: { label?: string }) {
  return (
    <div class="center" role="status" aria-live="polite">
      {label}
    </div>
  );
}

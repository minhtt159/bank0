// Format a whole number of seconds as mm:ss for the cooling-off countdown.
// Clamps negatives to 0 and pads both fields; minutes can exceed 99 (unlikely,
// cooling-off caps at 86400s but we don't truncate).
export function formatCountdown(totalSeconds: number): string {
  const s = Math.max(0, Math.floor(totalSeconds));
  const mins = Math.floor(s / 60);
  const secs = s % 60;
  return `${String(mins).padStart(2, "0")}:${String(secs).padStart(2, "0")}`;
}

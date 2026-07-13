// Human-facing labels for the enums the client API returns. Kept tiny and
// dependency-free; the API is the source of the underlying values.

import type { DisputeCategory, DisputeStatus } from "../api/types";

export const DISPUTE_CATEGORIES: { value: DisputeCategory; label: string }[] = [
  { value: "unrecognised", label: "I don't recognise this" },
  { value: "fraud", label: "Fraud" },
  { value: "wrong_amount", label: "Wrong amount" },
  { value: "duplicate", label: "Duplicate payment" },
  { value: "other", label: "Something else" },
];

export function disputeCategoryLabel(c: DisputeCategory): string {
  return DISPUTE_CATEGORIES.find((x) => x.value === c)?.label ?? c;
}

export function disputeStatusLabel(s: DisputeStatus): string {
  switch (s) {
    case "open":
      return "Open";
    case "under_review":
      return "Under review";
    case "resolved":
      return "Resolved";
    case "rejected":
      return "Rejected";
    default:
      return s;
  }
}

// Customer-facing label for a transfer's status. The two fraud-hold states get
// plain-language copy; everything else is title-cased (pending → "Pending").
export function transferStatusLabel(s: string): string {
  switch (s) {
    case "held":
      return "On hold — action needed";
    case "under_review":
      return "Under review";
    default:
      return s.charAt(0).toUpperCase() + s.slice(1);
  }
}

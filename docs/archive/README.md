# docs/archive — shipped feature specs (design rationale only)

These are the line-level specs for features that have **shipped**. They were planning
artifacts; their as-built behavior is the source of truth, not these files:

- Client API endpoints → [`../06-client-api.md`](../06-client-api.md)
- Operator console screens → [`../05-admin-ui.md`](../05-admin-ui.md)
- Schema / DB functions → `db/migrations/` + [`../02-data-model.md`](../02-data-model.md)

Kept for the **why** (design trade-offs, rejected alternatives). The open backlog and the
roadmap live in [`../specs/`](../specs/) — start at
[`../specs/spec-p3-roadmap.md`](../specs/spec-p3-roadmap.md).

| Spec | Feature | Shipped in |
|------|---------|-----------|
| `spec-change-password.md` | `POST /me/password` | `00018_change_password.sql` |
| `spec-guided-transfer-suggestion.md` | `GET /transfers/suggestion` | `00019_guided_scenarios.sql` |
| `spec-disputes.md` | disputes (client + admin + console) | `00020_disputes.sql` |
| `spec-sessions-devices.md` | `GET`/`DELETE /me/sessions` | `00021_session_device_label.sql` |
| `spec-self-service-profile.md` | `PATCH /me` profile edit | (no migration) |
| `spec-list-my-transfers.md` | `GET /transfers` across accounts | (no migration) |
| `spec-ledger-pagination-and-filters.md` | composite-cursor ledger + filters | (no migration) |

> Note: the envelope/`cursor.go` sections in the two pagination specs were **superseded**
> by the bare-array decision (see [`../09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md) §0.2).

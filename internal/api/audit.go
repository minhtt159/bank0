package api

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// audit records an operator action in admin_actions. Failures are logged, never
// fatal — an audit hiccup must not break the underlying operation (which is
// already done and itself recorded in the ledger for money moves).
func (s *Server) audit(ctx context.Context, actor db.SessionUser, action string, targetID *uuid.UUID, detail map[string]any) {
	raw := []byte("{}")
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			raw = b
		}
	}
	if err := s.pg.Queries.RecordAdminAction(ctx, sqlc.RecordAdminActionParams{
		ActorUserID: actor.UserID,
		Action:      action,
		TargetID:    targetID,
		Detail:      raw,
	}); err != nil {
		s.log.Warn("audit record failed", "action", action, "err", err)
	}
}

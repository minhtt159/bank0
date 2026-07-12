package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	template "github.com/minhtt159/bank0/web/template"
)

// ---- users (main panel + rail) ------------------------------------------

func (s *Server) consoleUsers(w http.ResponseWriter, r *http.Request) {
	canCreate := false
	if su, ok := userFromContext(r.Context()); ok {
		canCreate = canCreateUsers(su.Role)
	}
	s.html(w)
	_ = template.UsersPanel(canCreate).Render(r.Context(), w)
}

func (s *Server) consoleUsersResults(w http.ResponseWriter, r *http.Request) {
	q := searchQ(r)
	ts, cid := pageCursor(r)
	limit := s.consolePageLimit(r)
	rows, err := s.pg.Queries.SearchUsers(r.Context(), sqlc.SearchUsersParams{
		Q: q, Cursor: ts, CursorID: cid, PageLimit: limit + 1,
	})
	if err != nil {
		s.log.Error("search users", "err", err)
		http.Error(w, "users error", http.StatusInternalServerError)
		return
	}
	rows, lastTs, lastID, hasMore := paginate(rows, limit, func(u sqlc.SearchUsersRow) (time.Time, uuid.UUID) {
		return u.CreatedAt, u.ID
	})
	prev, next := pagerLinks(r, "/console/users/results", q, lastTs, lastID, hasMore)
	s.html(w)
	_ = template.UsersRows(rows, prev, next).Render(r.Context(), w)
}

func (s *Server) consoleNewUserForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, canCreateUsers); !ok {
		return
	}
	s.html(w)
	_ = template.CreateUserForm("").Render(r.Context(), w)
}

func (s *Server) consoleCreateUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canCreateUsers)
	if !ok {
		return
	}
	_ = r.ParseForm()
	role := strings.TrimSpace(r.PostFormValue("role"))
	if role == "" {
		role = string(sqlc.UserRoleCustomer)
	}
	if !validRole(role) {
		s.html(w)
		_ = template.CreateUserForm("Invalid role.").Render(r.Context(), w)
		return
	}
	id, err := s.pg.Queries.CreateUser(r.Context(), sqlc.CreateUserParams{
		Username:    strings.TrimSpace(r.PostFormValue("username")),
		Password:    r.PostFormValue("password"),
		FullName:    strings.TrimSpace(r.PostFormValue("full_name")),
		Email:       strOrNil(r.PostFormValue("email")),
		PhoneNumber: strOrNil(r.PostFormValue("phone_number")),
		Role:        sqlc.UserRole(role),
	})
	if err != nil {
		s.html(w)
		_ = template.CreateUserForm("Could not create user: "+s.dbFlash(r, err)).Render(r.Context(), w)
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "create_user", &id, map[string]any{
		"username": strings.TrimSpace(r.PostFormValue("username")), "role": role,
	})
	s.renderUserDetail(w, r, id, "User created.")
}

func (s *Server) consoleUserDetail(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	s.renderUserDetail(w, r, id, "")
}

func (s *Server) renderUserDetail(w http.ResponseWriter, r *http.Request, id uuid.UUID, flash string) {
	ctx := r.Context()
	u, err := s.pg.Queries.GetUserByID(ctx, id)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	accts, err := s.pg.Queries.ListAccountsByUser(ctx, id)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	role := ""
	if su, ok := userFromContext(ctx); ok {
		role = su.Role
	}
	s.html(w)
	_ = template.UserDetail(u, accts, canManageUsers(role), canActOnMoney(role), canCreateUsers(role), flash).Render(ctx, w)
}

// consoleSetInvites edits a user's remaining invitation quota. Operators and
// admins may adjust it (canCreateUsers). The count must be a non-negative integer.
func (s *Server) consoleSetInvites(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canCreateUsers)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	_ = r.ParseForm()
	n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("invites_remaining")))
	if err != nil || n < 0 {
		s.renderUserDetail(w, r, id, "Invitations must be a whole number of 0 or more.")
		return
	}
	if err := s.pg.Queries.SetInvitesRemaining(r.Context(), sqlc.SetInvitesRemainingParams{
		ID: id, InvitesRemaining: int32(n),
	}); err != nil {
		s.renderUserDetail(w, r, id, "Could not update invitations: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "set_invites", &id, map[string]any{"invites_remaining": n})
	s.renderUserDetail(w, r, id, fmt.Sprintf("Invitations remaining set to %d.", n))
}

func (s *Server) consoleUpdateUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canManageUsers)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	_ = r.ParseForm()
	var status sqlc.NullUserStatus
	if v := strings.TrimSpace(r.PostFormValue("status")); v != "" {
		status = sqlc.NullUserStatus{UserStatus: sqlc.UserStatus(v), Valid: true}
	}
	err = s.pg.Queries.UpdateUserInfo(r.Context(), sqlc.UpdateUserInfoParams{
		UserID:      id,
		FullName:    strOrNil(r.PostFormValue("full_name")),
		Email:       strOrNil(r.PostFormValue("email")),
		PhoneNumber: strOrNil(r.PostFormValue("phone_number")),
		Status:      status,
	})
	if err != nil {
		s.renderUserDetail(w, r, id, "Could not save: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "update_user", &id, map[string]any{"status": r.PostFormValue("status")})
	s.renderUserDetail(w, r, id, "Details saved.")
}

// consoleRevokeSessions force-revokes every active refresh token for a user
// (docs/06 "log out everywhere" / operator force-revoke). Admin only.
func (s *Server) consoleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireRole(w, r, canManageUsers)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	n, err := s.pg.RevokeUserRefresh(r.Context(), id)
	if err != nil {
		s.renderUserDetail(w, r, id, "Could not revoke sessions: "+s.dbFlash(r, err))
		return
	}
	refresh(w)
	s.audit(r.Context(), actor, "revoke_sessions", &id, map[string]any{"revoked": n})
	s.renderUserDetail(w, r, id, fmt.Sprintf("Revoked %d active app session(s).", n))
}

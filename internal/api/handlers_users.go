package api

import (
	"net/http"
	"strings"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/google/uuid"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

type createUserReq struct {
	Username    string  `json:"username"`
	Password    string  `json:"password"`
	FullName    string  `json:"full_name"`
	Email       *string `json:"email"`
	PhoneNumber *string `json:"phone_number"`
	Role        string  `json:"role"`
}

func validRole(r string) bool {
	switch sqlc.UserRole(r) {
	case sqlc.UserRoleCustomer, sqlc.UserRoleOperator, sqlc.UserRoleAdmin, sqlc.UserRoleAuditor:
		return true
	}
	return false
}

// CreateUser implements genadmin.ServerInterface.
func (s *Server) CreateUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, canManageUsers); !ok {
		return
	}
	var req createUserReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role == "" {
		req.Role = string(sqlc.UserRoleCustomer)
	}
	if !validRole(req.Role) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid role")
		return
	}
	id, err := s.pg.Queries.CreateUser(r.Context(), sqlc.CreateUserParams{
		Username:    req.Username,
		Password:    req.Password,
		FullName:    req.FullName,
		Email:       req.Email,
		PhoneNumber: req.PhoneNumber,
		Role:        sqlc.UserRole(req.Role),
	})
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// GetUser implements genadmin.ServerInterface.
func (s *Server) GetUser(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	u, err := s.pg.Queries.GetUserByID(r.Context(), uuid.UUID(id))
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// GetMe implements genclient.ServerInterface: the caller's own profile, resolved
// from the JWT subject. Client surface only (always behind requireJWT); the
// GetUserByID projection excludes the password hash.
func (s *Server) GetMe(w http.ResponseWriter, r *http.Request) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	u, err := s.pg.Queries.GetUserByID(r.Context(), subj)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

type updateMeReq struct {
	FullName    *string `json:"full_name"`
	Email       *string `json:"email"`
	PhoneNumber *string `json:"phone_number"`
}

// looksLikeEmail is a cheap shape check for a friendlier 422 than the raw DB CHECK
// message; the users.email CHECK constraint remains the authority.
func looksLikeEmail(e string) bool {
	at := strings.IndexByte(e, '@')
	return at > 0 && strings.IndexByte(e[at+1:], '.') > 0
}

// UpdateMe implements genclient.ServerInterface: self-service profile edit, scoped
// to the JWT subject (no user_id is accepted, so it's IDOR-proof by construction).
// It reuses update_user_info with password+status pinned nil, so the client surface
// can never change a password (use POST /me/password), unlock an account, or
// escalate role. See docs/specs/spec-self-service-profile.md.
func (s *Server) UpdateMe(w http.ResponseWriter, r *http.Request) {
	subj, ok := s.clientSubjectOr401(w, r)
	if !ok {
		return
	}
	var req updateMeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email != nil && *req.Email != "" && !looksLikeEmail(*req.Email) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_email", "email is not a valid address")
		return
	}
	if err := s.pg.Queries.UpdateUserInfo(r.Context(), sqlc.UpdateUserInfoParams{
		UserID:      subj,
		FullName:    req.FullName,
		Email:       req.Email,
		PhoneNumber: req.PhoneNumber,
		Password:    nil,                   // escalation guard: never settable here
		Status:      sqlc.NullUserStatus{}, // escalation guard: never settable here
	}); err != nil {
		s.mapDBError(w, r, err) // 23505 -> 409 (email/phone taken); 23514 -> 422 (bad email)
		return
	}
	u, err := s.pg.Queries.GetUserByID(r.Context(), subj)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Login implements genclient.ServerInterface.
func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !decodeJSON(w, r, &req) {
		return
	}
	id, role, uname, ok, err := s.pg.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		s.mapDBError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}
	// MFA-enabled users get a pending-login token instead of the real pair; the
	// client exchanges it (+ a TOTP/recovery code) at /auth/mfa/verify.
	if enabled, err := s.pg.MFAEnabled(r.Context(), id); err != nil {
		s.mapDBError(w, r, err)
		return
	} else if enabled {
		mfaTok, err := s.issueMFAToken(id, role, uname)
		if err != nil {
			s.logFor(r.Context()).Error("issue mfa token", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"user_id": id, "mfa_required": true, "mfa_token": mfaTok,
		})
		return
	}
	refresh := newSessionToken()
	if _, err := s.pg.IssueRefreshToken(r.Context(), id, hashToken(refresh),
		int(s.refreshTTL.Seconds()), r.UserAgent(), s.clientIP(r), ""); err != nil {
		s.mapDBError(w, r, err)
		return
	}
	s.writeTokenPair(w, id, role, uname, refresh, []string{"pwd"}, "")
}

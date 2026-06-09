package api

import (
	"net/http"

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
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

// GetUser implements genadmin.ServerInterface.
func (s *Server) GetUser(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	u, err := s.pg.Queries.GetUserByID(r.Context(), uuid.UUID(id))
	if err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// GetMe implements genclient.ServerInterface: the caller's own profile, resolved
// from the JWT subject. Client surface only (always behind requireJWT); the
// GetUserByID projection excludes the password hash.
func (s *Server) GetMe(w http.ResponseWriter, r *http.Request) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	u, err := s.pg.Queries.GetUserByID(r.Context(), subj)
	if err != nil {
		mapDBError(w, err)
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
	id, ok, err := s.pg.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		mapDBError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}
	u, err := s.pg.Queries.GetUserByID(r.Context(), id)
	if err != nil {
		mapDBError(w, err)
		return
	}
	token, exp, err := s.issueJWT(id, string(u.Role), u.Username)
	if err != nil {
		s.log.Error("issue jwt", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	refresh := newSessionToken()
	if _, err := s.pg.IssueRefreshToken(r.Context(), id, hashToken(refresh),
		int(s.refreshTTL.Seconds()), r.UserAgent(), clientIP(r)); err != nil {
		mapDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":       id,
		"token":         token,
		"token_type":    "Bearer",
		"expires_at":    exp,
		"refresh_token": refresh,
	})
}

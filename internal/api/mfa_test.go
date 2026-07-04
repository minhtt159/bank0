package api

import (
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"

	"github.com/minhtt159/bank0/internal/config"
	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// MFA + step-up HTTP flows. The test server gets a real AES key and a tiny
// step-up limit so gates are reachable.

func newMfaTestServer(t *testing.T, stepUpMaxAge time.Duration) (*httptest.Server, *db.Postgres) {
	t.Helper()
	if testDSN == "" {
		t.Skip("set TEST_DATABASE_DSN to run HTTP integration tests")
	}
	pg, err := db.NewPostgres(config.DatabaseConfig{
		DSN: testDSN, MaxOpenConns: 5, MaxIdleConns: 2, ConnTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	cfg := config.Config{
		App:    config.AppConfig{Name: "bank0", Version: "test", Env: "development"},
		Server: config.ServerConfig{Mode: "api", DefaultPageLimit: 25},
		Auth: config.AuthConfig{
			JWTSecret: "test-secret", JWTTTL: time.Hour, JWTIssuer: "bank0", JWTAudience: "bank0-client",
			StepUpLimitMinor: 5_000, StepUpMaxAge: stepUpMaxAge,
			MFAEncKey:   base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
			MFATokenTTL: 5 * time.Minute, MFALockMaxFail: 5, MFALockWindow: 15 * time.Minute,
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(NewServer(cfg, log, pg).Router())
	t.Cleanup(func() { ts.Close(); pg.Close() })
	return ts, pg
}

type loginBody struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
	MfaRequired  bool   `json:"mfa_required"`
	MfaToken     string `json:"mfa_token"`
}

func enrollAndConfirm(t *testing.T, tsURL, bearer string) (secret string, recovery []string) {
	t.Helper()
	auth := map[string]string{"Authorization": "Bearer " + bearer}
	r := postJSON(t, tsURL+"/auth/mfa/enroll", auth, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("enroll = %d: %s", r.StatusCode, body(t, r))
	}
	var enr struct {
		Secret     string `json:"secret"`
		OtpauthURI string `json:"otpauth_uri"`
	}
	decodeBody(t, r, &enr)
	if enr.Secret == "" || enr.OtpauthURI == "" {
		t.Fatalf("enroll body = %+v", enr)
	}
	code, err := totp.GenerateCode(enr.Secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	r = postJSON(t, tsURL+"/auth/mfa/confirm", auth, map[string]string{"code": code})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("confirm = %d: %s", r.StatusCode, body(t, r))
	}
	var rec struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	decodeBody(t, r, &rec)
	if len(rec.RecoveryCodes) != 10 {
		t.Fatalf("recovery codes = %d, want 10", len(rec.RecoveryCodes))
	}
	return enr.Secret, rec.RecoveryCodes
}

func TestHTTPMfaEnrollLoginVerifyFlow(t *testing.T) {
	ts, pg := newMfaTestServer(t, 5*time.Minute)
	_, uname := mkUser(t, pg, sqlc.UserRoleCustomer)

	// Plain login first (no MFA yet), then enroll + confirm.
	tok := bearerFor(t, ts.URL, uname, "pw")
	secret, recovery := enrollAndConfirm(t, ts.URL, tok)

	// Login now demands the second factor: no tokens, an mfa_token instead.
	r := postJSON(t, ts.URL+"/auth/login", nil, map[string]string{"username": uname, "password": "pw"})
	var lb loginBody
	decodeBody(t, r, &lb)
	if !lb.MfaRequired || lb.MfaToken == "" || lb.Token != "" || lb.RefreshToken != "" {
		t.Fatalf("mfa login body = %+v, want mfa_required + mfa_token only", lb)
	}
	// The mfa_token must NOT work as a bearer (audience isolation).
	if resp := get(t, http.DefaultClient, ts.URL+"/me",
		map[string]string{"Authorization": "Bearer " + lb.MfaToken}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("mfa_token as bearer = %d, want 401", resp.StatusCode)
	}

	// Wrong code -> 401.
	r = postJSON(t, ts.URL+"/auth/mfa/verify", nil, map[string]string{"mfa_token": lb.MfaToken, "code": "000000"})
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong code = %d, want 401", r.StatusCode)
	}
	// Live TOTP -> real pair; /me works.
	code, _ := totp.GenerateCode(secret, time.Now())
	r = postJSON(t, ts.URL+"/auth/mfa/verify", nil, map[string]string{"mfa_token": lb.MfaToken, "code": code})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("verify = %d: %s", r.StatusCode, body(t, r))
	}
	var pair loginBody
	decodeBody(t, r, &pair)
	if pair.Token == "" || pair.RefreshToken == "" {
		t.Fatalf("verify pair = %+v", pair)
	}
	if resp := get(t, http.DefaultClient, ts.URL+"/me",
		map[string]string{"Authorization": "Bearer " + pair.Token}); resp.StatusCode != http.StatusOK {
		t.Errorf("/me with verified token = %d", resp.StatusCode)
	}

	// Recovery code path: burn once, reuse fails.
	r = postJSON(t, ts.URL+"/auth/login", nil, map[string]string{"username": uname, "password": "pw"})
	decodeBody(t, r, &lb)
	r = postJSON(t, ts.URL+"/auth/mfa/verify", nil, map[string]string{"mfa_token": lb.MfaToken, "code": recovery[0]})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("recovery verify = %d", r.StatusCode)
	}
	r = postJSON(t, ts.URL+"/auth/login", nil, map[string]string{"username": uname, "password": "pw"})
	decodeBody(t, r, &lb)
	r = postJSON(t, ts.URL+"/auth/mfa/verify", nil, map[string]string{"mfa_token": lb.MfaToken, "code": recovery[0]})
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("burned recovery code reuse = %d, want 401", r.StatusCode)
	}
}

func TestHTTPStepUpGate(t *testing.T) {
	ts, pg := newMfaTestServer(t, 5*time.Minute)
	uid, uname := mkUser(t, pg, sqlc.UserRoleCustomer)
	from := mkAcct(t, pg, uid, 100_000)
	toID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	to := mkAcct(t, pg, toID, 0)

	// WITHOUT MFA: high-value transfer passes (no gate for un-enrolled users).
	tok := bearerFor(t, ts.URL, uname, "pw")
	hdr := map[string]string{"Authorization": "Bearer " + tok, "Idempotency-Key": uuid.NewString()}
	r := postJSON(t, ts.URL+"/transfers", hdr, map[string]any{
		"debit_account": from.String(), "credit_account": to.String(), "amount_minor": 6_000})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("no-MFA high-value = %d, want 200: %s", r.StatusCode, body(t, r))
	}

	// Enroll (still on the pwd token), then the SAME pwd-only token is gated.
	secret, _ := enrollAndConfirm(t, ts.URL, tok)
	key := uuid.NewString()
	hdr["Idempotency-Key"] = key
	r = postJSON(t, ts.URL+"/transfers", hdr, map[string]any{
		"debit_account": from.String(), "credit_account": to.String(), "amount_minor": 6_000})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("gated transfer = %d, want 403: %s", r.StatusCode, body(t, r))
	}
	if b := body(t, r); !strings.Contains(b, "step_up_required") {
		t.Fatalf("gate body = %s", b)
	}

	// Dynamic linking (Rec 14): an UNLINKED fresh OTP no longer authorizes a
	// gated transfer — the verify must commit to this exact (debit,credit,amount).
	lr := postJSON(t, ts.URL+"/auth/login", nil, map[string]string{"username": uname, "password": "pw"})
	var lb loginBody
	decodeBody(t, lr, &lb)
	code, _ := totp.GenerateCode(secret, time.Now())
	vr := postJSON(t, ts.URL+"/auth/mfa/verify", nil, map[string]string{"mfa_token": lb.MfaToken, "code": code})
	var unlinked loginBody
	decodeBody(t, vr, &unlinked)
	hdr = map[string]string{"Authorization": "Bearer " + unlinked.Token, "Idempotency-Key": key}
	r = postJSON(t, ts.URL+"/transfers", hdr, map[string]any{
		"debit_account": from.String(), "credit_account": to.String(), "amount_minor": 6_000})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("unlinked fresh OTP = %d, want 403 (WYSIWYS)", r.StatusCode)
	}

	// Linked verify to a DIFFERENT amount must not authorize this one.
	lr = postJSON(t, ts.URL+"/auth/login", nil, map[string]string{"username": uname, "password": "pw"})
	decodeBody(t, lr, &lb)
	code, _ = totp.GenerateCode(secret, time.Now())
	vr = postJSON(t, ts.URL+"/auth/mfa/verify", nil, map[string]any{
		"mfa_token": lb.MfaToken, "code": code,
		"link": map[string]any{"debit_account": from.String(), "credit_account": to.String(), "amount_minor": 9_999}})
	var wrongLink loginBody
	decodeBody(t, vr, &wrongLink)
	hdr = map[string]string{"Authorization": "Bearer " + wrongLink.Token, "Idempotency-Key": key}
	r = postJSON(t, ts.URL+"/transfers", hdr, map[string]any{
		"debit_account": from.String(), "credit_account": to.String(), "amount_minor": 6_000})
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong-amount link = %d, want 403", r.StatusCode)
	}

	// Correctly linked verify, retry with the SAME key -> exactly one transfer.
	lr = postJSON(t, ts.URL+"/auth/login", nil, map[string]string{"username": uname, "password": "pw"})
	decodeBody(t, lr, &lb)
	code, _ = totp.GenerateCode(secret, time.Now())
	vr = postJSON(t, ts.URL+"/auth/mfa/verify", nil, map[string]any{
		"mfa_token": lb.MfaToken, "code": code,
		"link": map[string]any{"debit_account": from.String(), "credit_account": to.String(), "amount_minor": 6_000}})
	var pair loginBody
	decodeBody(t, vr, &pair)

	hdr = map[string]string{"Authorization": "Bearer " + pair.Token, "Idempotency-Key": key}
	r = postJSON(t, ts.URL+"/transfers", hdr, map[string]any{
		"debit_account": from.String(), "credit_account": to.String(), "amount_minor": 6_000})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("post-step-up retry = %d: %s", r.StatusCode, body(t, r))
	}
	var n int
	if err := pg.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM transfers WHERE idempotency_key = $1`, key).Scan(&n); err != nil || n != 1 {
		t.Errorf("transfers for key = %d/%v, want exactly 1", n, err)
	}

	// New-payee gate: small amount to a non-beneficiary is still gated for the
	// pwd-only token; saving the payee first lifts it.
	small := map[string]any{"debit_account": from.String(), "credit_account": to.String(), "amount_minor": 500}
	hdrPwd := map[string]string{"Authorization": "Bearer " + tok, "Idempotency-Key": uuid.NewString()}
	r = postJSON(t, ts.URL+"/transfers", hdrPwd, small)
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("new-payee small = %d, want 403", r.StatusCode)
	}
	var toIban string
	_ = pg.Pool.QueryRow(t.Context(), `SELECT iban FROM accounts WHERE id=$1`, to).Scan(&toIban)
	if _, err := pg.Pool.Exec(t.Context(), `SELECT add_beneficiary($1,'pal',$2)`, uid, toIban); err != nil {
		t.Fatalf("add beneficiary: %v", err)
	}
	hdrPwd["Idempotency-Key"] = uuid.NewString()
	r = postJSON(t, ts.URL+"/transfers", hdrPwd, small)
	if r.StatusCode != http.StatusOK {
		t.Errorf("known-payee small = %d, want 200: %s", r.StatusCode, body(t, r))
	}
}

//go:build e2e

// Package e2e is a black-box, cross-surface end-to-end test of the REAL deployed
// two-binary topology: one compiled binary run twice — once as APP_SERVER_MODE=portal
// (admin API + operator console) and once as APP_SERVER_MODE=api (client JSON API) —
// both pointed at one throwaway Postgres. It is Tier A of docs/specs/spec-e2e-harness.md.
//
// Everything here is behind `//go:build e2e`, so a plain `go test ./...`,
// `go build ./...`, and `go vet ./...` never compile this package. Run it with:
//
//	export TEST_DATABASE_DSN='postgres://admin:admin@localhost:5546/bank0?sslmode=disable'
//	go test -tags e2e ./internal/e2e/ -v
//
// Like the in-process integration tests (internal/db, internal/api) the suite is
// DSN-gated: with TEST_DATABASE_DSN unset every test SKIPs. TestMain migrates the
// target DB fresh (via the compiled binary's `migrate up`), builds the binary once,
// grabs two free ports, launches the two processes, and polls /health on each.
package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// jwtSecret is shared by BOTH processes so a JWT minted by the api binary verifies
// the same way everywhere (and so the test can reason about a single signing key).
const jwtSecret = "tierA-e2e-shared-secret"

// proc bundles a running server process with the captured stderr so a failing test
// can dump the binary's logs.
type proc struct {
	mode    string
	port    int
	baseURL string
	cmd     *exec.Cmd
	logs    *syncBuffer
}

// env is the shared harness state, set up once in TestMain and read by every test.
type env struct {
	repoRoot string
	dsn      string
	binary   string
	portal   *proc // APP_SERVER_MODE=portal
	api      *proc // APP_SERVER_MODE=api
}

var harness *env

func TestMain(m *testing.M) {
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		// DSN-gated, exactly like the internal/db + internal/api integration suites:
		// with no database the whole package is a no-op (every test SKIPs).
		fmt.Fprintln(os.Stderr, "[e2e] TEST_DATABASE_DSN unset — skipping the Tier A e2e suite")
		os.Exit(m.Run())
	}

	e, cleanup, err := setup(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[e2e] setup failed: %v\n", err)
		cleanup()
		os.Exit(1)
	}
	harness = e
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// repoRootFromCaller derives the repository root from this source file's location:
// internal/e2e/harness_test.go -> ../.. The binary reads config.yaml AND
// api/openapi.yaml relative to its working directory, so cmd.Dir must be this root.
func repoRootFromCaller() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	return filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
}

// setup builds the binary, migrates the DB, and launches both processes. The
// returned cleanup is ALWAYS safe to call (even on a partial failure) and kills any
// process that was started.
func setup(dsn string) (*env, func(), error) {
	var cleanups []func()
	cleanup := func() {
		// Run cleanups in reverse registration order (LIFO).
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}

	root, err := repoRootFromCaller()
	if err != nil {
		return nil, cleanup, err
	}

	binDir, err := os.MkdirTemp("", "bank0-e2e-")
	if err != nil {
		return nil, cleanup, fmt.Errorf("mkdtemp: %w", err)
	}
	cleanups = append(cleanups, func() { _ = os.RemoveAll(binDir) })
	binary := filepath.Join(binDir, "bank0")

	// 1) Build the binary once.
	build := exec.Command("go", "build", "-o", binary, "./cmd/app")
	build.Dir = root
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		return nil, cleanup, fmt.Errorf("go build: %w\n%s", err, out)
	}

	// 2) Migrate the throwaway DB fresh via the binary's own `migrate up`. (We use
	// the binary rather than importing internal/migrate so the harness exercises the
	// exact same code path the deploy uses.)
	mig := exec.Command(binary, "migrate", "up")
	mig.Dir = root
	mig.Env = append(os.Environ(), "APP_DATABASE_DSN="+dsn)
	if out, err := mig.CombinedOutput(); err != nil {
		return nil, cleanup, fmt.Errorf("migrate up: %w\n%s", err, out)
	}

	// 3) Grab two free ports: bind :0, read the kernel-assigned port, close, reuse.
	// Mirrors web/app/e2e/global-setup.ts's port-grab approach.
	portalPort, err := freePort()
	if err != nil {
		return nil, cleanup, fmt.Errorf("free port (portal): %w", err)
	}
	apiPort, err := freePort()
	if err != nil {
		return nil, cleanup, fmt.Errorf("free port (api): %w", err)
	}

	// 4) Launch both processes against the same DSN + JWT secret, rate limit 0.
	portal, err := startServer(root, binary, dsn, "portal", portalPort)
	if err != nil {
		return nil, cleanup, fmt.Errorf("start portal: %w", err)
	}
	cleanups = append(cleanups, func() { stopServer(portal) })

	api, err := startServer(root, binary, dsn, "api", apiPort)
	if err != nil {
		return nil, cleanup, fmt.Errorf("start api: %w", err)
	}
	cleanups = append(cleanups, func() { stopServer(api) })

	// 5) Poll /health on each until ready (served on every surface — see Router()).
	for _, p := range []*proc{portal, api} {
		if err := waitHealth(p, 15*time.Second); err != nil {
			return nil, cleanup, fmt.Errorf("%s never became healthy: %w\n--- %s stderr ---\n%s",
				p.mode, err, p.mode, p.logs.String())
		}
	}

	return &env{
		repoRoot: root, dsn: dsn, binary: binary,
		portal: portal, api: api,
	}, cleanup, nil
}

// freePort binds an ephemeral port, records the number, and releases it. There is a
// small race between release and the child re-binding, but the OS does not recycle a
// just-freed ephemeral port that fast in practice (same trick as the Playwright setup).
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func startServer(root, binary, dsn, mode string, port int) (*proc, error) {
	logs := &syncBuffer{}
	cmd := exec.Command(binary, "serve")
	cmd.Dir = root // config.yaml + api/openapi.yaml are read relative to the cwd
	cmd.Stderr = logs
	cmd.Stdout = logs
	cmd.Env = append(os.Environ(),
		"APP_DATABASE_DSN="+dsn,
		"APP_SERVER_MODE="+mode,
		fmt.Sprintf("APP_SERVER_PORT=%d", port),
		"APP_AUTH_JWT_SECRET="+jwtSecret,
		"APP_SERVER_RATE_LIMIT_PER_MIN=0", // disable the /auth/* limiter (many logins in tests)
		"APP_ADMIN_RUN_MAINTENANCE=false", // no background ticking under test
		"APP_LOGGING_ENCODING=json",
	)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &proc{
		mode:    mode,
		port:    port,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		cmd:     cmd,
		logs:    logs,
	}, nil
}

func stopServer(p *proc) {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
}

// waitHealth polls GET /health until it returns 200 or the timeout elapses. It also
// fails fast if the process has already exited (e.g. bad config, DSN unreachable).
func waitHealth(p *proc, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.cmd.ProcessState != nil && p.cmd.ProcessState.Exited() {
			return fmt.Errorf("process exited before becoming healthy")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/health", nil)
		resp, err := client.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s", timeout)
}

// requireHarness skips a test when the suite is not provisioned (no DSN).
func requireHarness(t *testing.T) *env {
	t.Helper()
	if harness == nil {
		t.Skip("set TEST_DATABASE_DSN to run the Tier A cross-surface e2e suite")
	}
	return harness
}

// syncBuffer is a goroutine-safe io.Writer for capturing each process's combined
// output (os/exec writes from its own goroutine).
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

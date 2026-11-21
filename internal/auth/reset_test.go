package auth_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
	"younithm/internal/db"
)

type resetEnv struct {
	*env
	db *sql.DB
}

func newResetEnv(t *testing.T) *resetEnv {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	svc := auth.Mount(r, d, auth.Options{})
	r.With(auth.RequireSession).Get("/t/session", func(http.ResponseWriter, *http.Request) {})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &resetEnv{env: &env{t: t, srv: srv, svc: svc}, db: d}
}

func TestResetAdmin(t *testing.T) {
	e := newResetEnv(t)
	e.setup()
	ctx := t.Context()

	// Lock-out residue.
	_, _ = e.db.Exec(`INSERT INTO auth_throttle (key, fails, locked_until, updated_at) VALUES ('ip:x', 9, '2999-01-01T00:00:00.000Z', 'x')`)

	if _, err := e.svc.ResetAdmin(ctx, "short"); err == nil {
		t.Fatal("short password accepted")
	}

	// Sessions die, lockouts clear, the new password works.
	name, err := e.svc.ResetAdmin(ctx, "brand new password")
	if err != nil || name != "admin" {
		t.Fatalf("%q %v", name, err)
	}
	if resp, _ := e.do("GET", "/t/session", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("old session survived: %d", resp.StatusCode)
	}
	var n int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM auth_throttle`).Scan(&n)
	if n != 0 {
		t.Fatal("throttle not cleared")
	}
	if _, _, err := e.svc.Login(ctx, "1.1.1.1", "", "admin", pw); err == nil {
		t.Fatal("old password still works")
	}
	if _, _, err := e.svc.Login(ctx, "1.1.1.1", "", "admin", "brand new password"); err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
}

func TestResetAdminNoAdmin(t *testing.T) {
	e := newResetEnv(t)
	if _, err := e.svc.ResetAdmin(t.Context(), "long enough password"); err != auth.ErrNoAdmin {
		t.Fatalf("got %v", err)
	}
}

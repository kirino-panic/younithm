package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
	"younithm/internal/db"
)

// A signed-in request must not wait for the write connection: background work
// can hold it for minutes, and every page load checks the session cookie.
func TestSessionLookupWhileWriterBusy(t *testing.T) {
	pools, err := db.OpenPools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pools.Close() })
	if err := db.Migrate(pools.Write); err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	svc := auth.Mount(r, pools.Write, auth.Options{Read: pools.Read})
	r.With(auth.RequireSession).Get("/api/v1/admin-only", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	e := &env{t: t, srv: srv, svc: svc}
	e.setup()

	// Hold the only write connection, as a long background job would.
	tx, err := pools.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE admin_user SET username = username`); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		resp, _ := e.do("GET", "/api/v1/admin-only", nil, nil)
		done <- resp.StatusCode
	}()
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("status %d, want 200", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("signed-in request waited for the write connection")
	}
}

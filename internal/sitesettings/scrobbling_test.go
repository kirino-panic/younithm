package sitesettings

import (
	"testing"

	"younithm/internal/db"
)

func TestScrobbleAuth(t *testing.T) {
	pools, err := db.OpenPools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer pools.Close()
	if err := db.Migrate(pools.Write); err != nil {
		t.Fatal(err)
	}
	s := NewStore(pools.Read, pools.Write)
	a := ScrobbleAuth{Site: s}
	ctx := t.Context()

	if _, _, err := a.AuthenticateToken(ctx, ""); err == nil {
		t.Fatal("no admin yet: refused")
	}
	if _, err := pools.Write.Exec(`INSERT INTO admin_user (username, password_hash) VALUES ('ada', 'x')`); err != nil {
		t.Fatal(err)
	}
	if !s.RequireScrobbleToken(ctx) {
		t.Fatal("unset means a token is required")
	}
	if _, _, err := a.AuthenticateToken(ctx, ""); err == nil {
		t.Fatal("required, none given: refused")
	}

	if err := s.SetScrobbleToken(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	if _, name, err := a.AuthenticateToken(ctx, ""); err != nil || name != "ada" {
		t.Fatalf("open: %q %v", name, err)
	}

	if err := s.SetScrobbleToken(ctx, true, "has space"); err == nil {
		t.Fatal("spaces refused")
	}
	if err := s.SetScrobbleToken(ctx, true, "hunter2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.AuthenticateToken(ctx, "hunter2"); err != nil {
		t.Fatalf("chosen token: %v", err)
	}
	if _, _, err := a.AuthenticateToken(ctx, "wrong"); err == nil {
		t.Fatal("wrong token refused")
	}
	// Off and on again keeps the stored token.
	_ = s.SetScrobbleToken(ctx, false, "")
	if err := s.SetScrobbleToken(ctx, true, ""); err != nil {
		t.Fatalf("re-enable with stored token: %v", err)
	}
	if _, _, err := a.AuthenticateToken(ctx, "hunter2"); err != nil {
		t.Fatalf("token kept: %v", err)
	}
}

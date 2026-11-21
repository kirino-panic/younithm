package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"younithm/internal/auth"
	"younithm/internal/db"
)

func seedAdmin(t *testing.T, dir string) {
	t.Helper()
	d, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	svc := auth.New(d)
	if _, err := svc.Setup(context.Background(), "root", "old password 123"); err != nil {
		t.Fatal(err)
	}
}

func canLogin(t *testing.T, dir, pw string) bool {
	t.Helper()
	d, err := db.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, _, err = auth.New(d).Login(context.Background(), "127.0.0.1", "", "root", pw)
	return err == nil
}

func TestResetAdminCLI(t *testing.T) {
	dir := t.TempDir()
	seedAdmin(t, dir)

	var out bytes.Buffer
	if err := runResetAdmin(dir, []string{"--stdin"}, strings.NewReader("new password 456\n"), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), `"root"`) || strings.Contains(out.String(), "new password 456") {
		t.Fatalf("output: %q", out.String())
	}
	if canLogin(t, dir, "old password 123") || !canLogin(t, dir, "new password 456") {
		t.Fatal("password not reset")
	}

	// No trailing newline, CRLF, and the interactive prompt path.
	out.Reset()
	if err := runResetAdmin(dir, []string{}, strings.NewReader("third password 789\r\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "New admin password:") || !canLogin(t, dir, "third password 789") {
		t.Fatalf("%q", out.String())
	}
	if err := runResetAdmin(dir, []string{"--stdin"}, strings.NewReader("fourth password 000"), &out); err != nil || !canLogin(t, dir, "fourth password 000") {
		t.Fatalf("no-newline stdin: %v", err)
	}
}

func TestResetAdminCLIErrors(t *testing.T) {
	var out bytes.Buffer
	dir := t.TempDir()
	// No admin yet.
	if err := runResetAdmin(dir, []string{"--stdin"}, strings.NewReader("long enough password\n"), &out); err == nil {
		t.Fatal("expected no-admin error")
	}
	seedAdmin(t, dir)
	if err := runResetAdmin(dir, []string{"--stdin"}, strings.NewReader("short\n"), &out); err == nil {
		t.Fatal("short password accepted")
	}
	if err := runResetAdmin(dir, []string{"--stdin"}, strings.NewReader(""), &out); err == nil {
		t.Fatal("empty stdin accepted")
	}
	// Passwords on argv are refused.
	if err := runResetAdmin(dir, []string{"hunter2hunter2"}, strings.NewReader(""), &out); err == nil {
		t.Fatal("argv password accepted")
	}
	if !canLogin(t, dir, "old password 123") {
		t.Fatal("failed resets must leave the password alone")
	}
}

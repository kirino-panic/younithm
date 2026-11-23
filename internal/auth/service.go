// Package auth implements single-admin authentication: first-run setup (the
// first visitor creates the admin), Argon2id passwords, server-side sessions,
// CSRF, login throttling and step-up re-authentication. See docs/security.md.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	timeFmt = "2006-01-02T15:04:05.000Z"

	sessionAbsolute = 30 * 24 * time.Hour
	sessionIdle     = 7 * 24 * time.Hour
	stepUpWindow    = 5 * time.Minute
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrLocked             = errors.New("too many attempts")
	ErrSetupDone          = errors.New("setup already completed")
	ErrNoSession          = errors.New("no session")
)

// Service owns all auth state. Obtain one via New; Mount wires it to HTTP.
type Service struct {
	db *sql.DB
	// read answers session lookups. Every request with a cookie makes one, so
	// it must not wait behind the single write connection, which background
	// work can hold for a long time. Defaults to db.
	read     *sql.DB
	touching sync.Map // session ids whose idle timer is being saved
	now      func() time.Time
	// hasher is swappable in tests (argon2 at production cost is slow).
	hash   func(string) (string, error)
	verify func(pw, enc string) (bool, error)
}

func New(db *sql.DB) *Service {
	return &Service{db: db, read: db, now: time.Now, hash: HashPassword, verify: VerifyPassword}
}

func ts(t time.Time) string { return t.UTC().Format(timeFmt) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(timeFmt, s)
	return t
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func ctEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// User is the admin account as seen by handlers.
type User struct {
	ID       int64
	Username string
}

// ---- setup -----------------------------------------------------------------

// NeedsSetup reports whether no admin exists yet.
func (s *Service) NeedsSetup(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_user`).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// LogSetupPending notes at startup that no admin exists yet.
func (s *Service) LogSetupPending(ctx context.Context) error {
	need, err := s.NeedsSetup(ctx)
	if err != nil || !need {
		return err
	}
	log.Printf("auth: no admin account yet. Open the web UI to create one.")
	return nil
}

// Setup creates the admin account; the first one to call it gets it. It
// refuses once an admin exists.
func (s *Service) Setup(ctx context.Context, username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	ph, err := s.hash(password)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_user`).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, ErrSetupDone
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO admin_user (username, password_hash) VALUES (?, ?)`, username, ph)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &User{ID: id, Username: username}, nil
}

func validateUsername(u string) error {
	if len(u) < 1 || len(u) > 64 {
		return errors.New("username must be 1-64 characters")
	}
	return nil
}

// ---- throttling ------------------------------------------------------------

const (
	throttleFree = 5 // failures before any lockout
	throttleBase = 30 * time.Second
	// Both caps are short: this is a personal app, and a few typos shouldn't
	// cost long. Doubling waits still make guessing impractically slow.
	throttleMaxIP   = 15 * time.Minute
	throttleMaxUser = 15 * time.Minute
)

// checkThrottle returns ErrLocked (and the remaining wait) if key is locked.
func (s *Service) checkThrottle(ctx context.Context, key string) (time.Duration, error) {
	var until sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT locked_until FROM auth_throttle WHERE key = ?`, key).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if until.Valid {
		if d := parseTS(until.String).Sub(s.now()); d > 0 {
			return d, ErrLocked
		}
	}
	return 0, nil
}

// recordFailure bumps the counter and applies exponential backoff:
// 5 free failures, then 30s, 60s, 120s ... up to max.
func (s *Service) recordFailure(ctx context.Context, key string, max time.Duration) {
	now := s.now()
	var fails int
	_ = s.db.QueryRowContext(ctx, `SELECT fails FROM auth_throttle WHERE key = ?`, key).Scan(&fails)
	fails++
	var locked any
	if fails > throttleFree {
		shift := fails - throttleFree - 1
		if shift > 20 {
			shift = 20
		}
		d := throttleBase << shift
		if d > max || d <= 0 {
			d = max
		}
		locked = ts(now.Add(d))
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_throttle (key, fails, locked_until, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET fails = excluded.fails, locked_until = excluded.locked_until, updated_at = excluded.updated_at`,
		key, fails, locked, ts(now))
	if err != nil {
		log.Printf("auth: record failure: %v", err)
	}
}

func (s *Service) clearThrottle(ctx context.Context, key string) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM auth_throttle WHERE key = ?`, key)
}

// ---- login -----------------------------------------------------------------

// LoginResult is a freshly created session.
type LoginResult struct {
	Token   string // raw cookie value; never stored
	CSRF    string
	Expires time.Time
	User    User
}

// Login verifies credentials and starts a session. ip is used for per-IP
// throttling.
func (s *Service) Login(ctx context.Context, ip, ua, username, password string) (*LoginResult, time.Duration, error) {
	ipKey := "ip:" + ip
	userKey := "user:" + strings.ToLower(strings.TrimSpace(username))
	for _, k := range []string{ipKey, userKey} {
		if d, err := s.checkThrottle(ctx, k); err != nil {
			return nil, d, err
		}
	}
	fail := func() (*LoginResult, time.Duration, error) {
		s.recordFailure(ctx, ipKey, throttleMaxIP)
		s.recordFailure(ctx, userKey, throttleMaxUser)
		return nil, 0, ErrInvalidCredentials
	}

	var (
		u     User
		phash string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash FROM admin_user WHERE username = ? COLLATE NOCASE`,
		strings.TrimSpace(username)).Scan(&u.ID, &u.Username, &phash)
	if errors.Is(err, sql.ErrNoRows) {
		_, _ = s.verify(password, dummyHash) // equalize timing
		return fail()
	}
	if err != nil {
		return nil, 0, err
	}
	ok, _ := s.verify(password, phash)
	if !ok {
		return fail()
	}
	s.clearThrottle(ctx, ipKey)
	s.clearThrottle(ctx, userKey)
	res, err := s.createSession(ctx, u, ip, ua)
	return res, 0, err
}

func (s *Service) createSession(ctx context.Context, u User, ip, ua string) (*LoginResult, error) {
	tok, csrf := randToken(32), randToken(32)
	now := s.now()
	exp := now.Add(sessionAbsolute)
	if len(ua) > 300 {
		ua = ua[:300]
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_session (token_hash, user_id, csrf_token, created_at, last_seen_at, expires_at, ip, user_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sha256hex(tok), u.ID, csrf, ts(now), ts(now), ts(exp), ip, ua)
	if err != nil {
		return nil, err
	}
	// Opportunistic cleanup of dead sessions.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM auth_session WHERE expires_at < ? OR last_seen_at < ?`,
		ts(now), ts(now.Add(-sessionIdle)))
	return &LoginResult{Token: tok, CSRF: csrf, Expires: exp, User: u}, nil
}

// ---- sessions --------------------------------------------------------------

// Session is a validated, live session.
type Session struct {
	ID          int64
	User        User
	CSRF        string
	StepUpUntil time.Time
}

func (s *Session) SteppedUp(now time.Time) bool { return now.Before(s.StepUpUntil) }

// LookupSession validates a raw cookie token and slides the idle timer.
func (s *Service) LookupSession(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	var (
		ss      Session
		exp, ls string
		su      sql.NullString
	)
	err := s.read.QueryRowContext(ctx,
		`SELECT s.id, s.csrf_token, s.expires_at, s.last_seen_at, s.step_up_until, u.id, u.username
		 FROM auth_session s JOIN admin_user u ON u.id = s.user_id WHERE s.token_hash = ?`,
		sha256hex(token)).Scan(&ss.ID, &ss.CSRF, &exp, &ls, &su, &ss.User.ID, &ss.User.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if now.After(parseTS(exp)) || now.After(parseTS(ls).Add(sessionIdle)) {
		s.later(ss.ID, `DELETE FROM auth_session WHERE id = ?`, ss.ID)
		return nil, ErrNoSession
	}
	if su.Valid {
		ss.StepUpUntil = parseTS(su.String)
	}
	// Only write when it matters, to keep reads cheap.
	if now.Sub(parseTS(ls)) > time.Minute {
		s.later(ss.ID, `UPDATE auth_session SET last_seen_at = ? WHERE id = ?`, ts(now), ss.ID)
	}
	return &ss, nil
}

// later runs a session's housekeeping write in the background, one at a time
// per session, so a request never waits for the write connection. A write
// that cannot run in time is dropped; the next request tries again.
func (s *Service) later(id int64, query string, args ...any) {
	if _, busy := s.touching.LoadOrStore(id, struct{}{}); busy {
		return
	}
	go func() {
		defer s.touching.Delete(id)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = s.db.ExecContext(ctx, query, args...)
	}()
}

func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_session WHERE token_hash = ?`, sha256hex(token))
	return err
}

// RevokeOtherSessions deletes all of the user's sessions except keepID.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID, keepID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_session WHERE user_id = ? AND id != ?`, userID, keepID)
	return err
}

// ---- step-up & password ----------------------------------------------------

// StepUp re-verifies the password for the session and opens a short window in
// which sensitive actions are allowed.
func (s *Service) StepUp(ctx context.Context, sess *Session, ip, password string) (time.Time, error) {
	key := "user:" + strings.ToLower(sess.User.Username)
	if _, err := s.checkThrottle(ctx, key); err != nil {
		return time.Time{}, err
	}
	if err := s.verifyPassword(ctx, sess.User.ID, password); err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			s.recordFailure(ctx, key, throttleMaxUser)
			s.recordFailure(ctx, "ip:"+ip, throttleMaxIP)
		}
		return time.Time{}, err
	}
	s.clearThrottle(ctx, key)
	until := s.now().Add(stepUpWindow)
	_, err := s.db.ExecContext(ctx, `UPDATE auth_session SET step_up_until = ? WHERE id = ?`, ts(until), sess.ID)
	return until, err
}

// verifyPassword checks the account's current password.
func (s *Service) verifyPassword(ctx context.Context, uid int64, password string) error {
	var phash string
	if err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM admin_user WHERE id = ?`, uid).Scan(&phash); err != nil {
		return err
	}
	if ok, _ := s.verify(password, phash); !ok {
		return ErrInvalidCredentials
	}
	return nil
}

// ChangePassword verifies the current password and sets a new one. With
// signOutOthers, every other session is revoked.
func (s *Service) ChangePassword(ctx context.Context, sess *Session, current, next string, signOutOthers bool) error {
	if err := validatePassword(next); err != nil {
		return err
	}
	if err := s.verifyPassword(ctx, sess.User.ID, current); err != nil {
		return err
	}
	nh, err := s.hash(next)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE admin_user SET password_hash = ?, updated_at = ? WHERE id = ?`,
		nh, ts(s.now()), sess.User.ID); err != nil {
		return err
	}
	if !signOutOthers {
		return nil
	}
	return s.RevokeOtherSessions(ctx, sess.User.ID, sess.ID)
}

// ErrUsernameTaken is returned when another account already has the name.
var ErrUsernameTaken = errors.New("username taken")

// ChangeUsername verifies the current password and renames the account. Login
// matches names case-insensitively, so a case-only change is allowed.
func (s *Service) ChangeUsername(ctx context.Context, sess *Session, current, next string) (string, error) {
	next = strings.TrimSpace(next)
	if err := validateUsername(next); err != nil {
		return "", err
	}
	if err := s.verifyPassword(ctx, sess.User.ID, current); err != nil {
		return "", err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_user WHERE username = ? COLLATE NOCASE AND id != ?`,
		next, sess.User.ID).Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "", ErrUsernameTaken
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE admin_user SET username = ?, updated_at = ? WHERE id = ?`,
		next, ts(s.now()), sess.User.ID); err != nil {
		return "", err
	}
	// Lockouts are keyed by name; don't carry the old one's over.
	s.clearThrottle(ctx, "user:"+strings.ToLower(next))
	return next, nil
}

func (s *Service) String() string { return fmt.Sprintf("auth.Service") }

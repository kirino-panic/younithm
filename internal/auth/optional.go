package auth

import "net/http"

// IsAdminSession reports whether r carries a valid admin session cookie. It
// never rejects: it is for handlers that serve everyone but treat the admin
// differently (e.g. showing sections hidden from the public). Returns false before the auth service is mounted.
func IsAdminSession(r *http.Request) bool {
	d := defaultSvc.Load()
	if d == nil {
		return false
	}
	p, _, _ := d.mw.sessionPrincipal(r)
	return p != nil
}

package config

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// EnvBasePath serves the app under a path instead of a whole (sub)domain,
// e.g. YOUNITHM_BASE_PATH=/younithm for https://example.com/younithm/.
const EnvBasePath = "YOUNITHM_BASE_PATH"

// ParseBasePath normalizes a base path to "" (the root) or "/a/b" with a
// leading slash and no trailing one. Only URL-safe path characters are
// allowed, so the value can go into HTML and cookies as is.
func ParseBasePath(s string) (string, error) {
	s = strings.Trim(strings.TrimSpace(s), "/")
	if s == "" {
		return "", nil
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%s: %q is not a clean path", EnvBasePath, s)
		}
		for _, c := range seg {
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
				c == '-' || c == '_' || c == '.' || c == '~'
			if !ok {
				return "", fmt.Errorf("%s: %q may only use letters, digits, - _ . ~ and /", EnvBasePath, s)
			}
		}
	}
	return "/" + s, nil
}

var basePath = sync.OnceValue(func() string {
	p, _ := ParseBasePath(os.Getenv(EnvBasePath)) // Validate fails startup on a bad value
	return p
})

// BasePath is the configured base path: "" at the root, else "/younithm".
// Prefix it to any URL the server hands to a browser.
func BasePath() string { return basePath() }

package config

import (
	"log"
	"os"
	"time"
)

// DefaultDedupeWindow is the live-scrobble duplicate tolerance.
const DefaultDedupeWindow = 2 * time.Second

// DedupeWindow returns the live-scrobble dedupe tolerance: two listens of the
// same normalized artist+title whose played_at differ by at most this much
// count as one. Env YOUNITHM_DEDUPE_WINDOW, a Go duration ("2s", "500ms");
// "0" is invalid and falls back to the default, as does an unparsable or
// negative value (with a log line). Capped at 1h.
func DedupeWindow() time.Duration {
	v := os.Getenv("YOUNITHM_DEDUPE_WINDOW")
	if v == "" {
		return DefaultDedupeWindow
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 || d > time.Hour {
		log.Printf("config: invalid YOUNITHM_DEDUPE_WINDOW %q; using %s", v, DefaultDedupeWindow)
		return DefaultDedupeWindow
	}
	return d
}

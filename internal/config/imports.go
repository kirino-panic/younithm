package config

import (
	"log"
	"os"
	"strconv"
	"time"
)

// Import settings (see docs/imports.md).
const (
	EnvImportDedupeWindow = "YOUNITHM_IMPORT_DEDUPE_WINDOW"
	EnvImportMaxBytes     = "YOUNITHM_IMPORT_MAX_BYTES"

	// DefaultImportDedupeWindow is the tolerance used when an imported
	// listen is compared with scrobbles already stored. It is wider than the
	// live window (DefaultDedupeWindow) because exporters disagree about
	// timestamps by seconds: Spotify's start time is derived from stream end
	// minus ms_played, Last.fm and ListenBrainz clients may stamp the start
	// or the submission time, and live scrobblers add their own latency.
	DefaultImportDedupeWindow = 30 * time.Second

	// DefaultImportMaxBytes caps one uploaded import file (1 GiB).
	DefaultImportMaxBytes int64 = 1 << 30
)

// ImportDedupeWindow returns the import-time dedupe tolerance from
// YOUNITHM_IMPORT_DEDUPE_WINDOW (a Go duration). Invalid, non-positive or
// over-1h values fall back to the default with a log line.
func ImportDedupeWindow() time.Duration {
	v := os.Getenv(EnvImportDedupeWindow)
	if v == "" {
		return DefaultImportDedupeWindow
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 || d > time.Hour {
		log.Printf("config: invalid %s %q; using %s", EnvImportDedupeWindow, v, DefaultImportDedupeWindow)
		return DefaultImportDedupeWindow
	}
	return d
}

// ImportMaxBytes returns the upload size cap from YOUNITHM_IMPORT_MAX_BYTES.
func ImportMaxBytes() int64 {
	v := os.Getenv(EnvImportMaxBytes)
	if v == "" {
		return DefaultImportMaxBytes
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		log.Printf("config: invalid %s %q; using %d", EnvImportMaxBytes, v, DefaultImportMaxBytes)
		return DefaultImportMaxBytes
	}
	return n
}

// EnvDebugImportLimit is a DEBUG-ONLY switch: when set to N > 0, an import
// only processes the N most recent listens in the file (by played-at time).
// Leave it unset in normal use.
const EnvDebugImportLimit = "YOUNITHM_DEBUG_IMPORT_LIMIT"

// DebugImportLimit returns the debug cap on listens per import, or 0 (off).
func DebugImportLimit() int {
	v := os.Getenv(EnvDebugImportLimit)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("config: invalid %s %q; ignoring", EnvDebugImportLimit, v)
		return 0
	}
	return n
}

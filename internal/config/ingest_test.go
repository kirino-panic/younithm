package config

import (
	"testing"
	"time"
)

func TestDedupeWindow(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": 2 * time.Second, "5s": 5 * time.Second, "500ms": 500 * time.Millisecond,
		"0": 2 * time.Second, "-1s": 2 * time.Second, "junk": 2 * time.Second, "3h": 2 * time.Second,
	} {
		t.Setenv("YOUNITHM_DEDUPE_WINDOW", in)
		if got := DedupeWindow(); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

package config

import (
	"testing"
	"time"
)

func TestImportSettings(t *testing.T) {
	if ImportDedupeWindow() != 30*time.Second || ImportMaxBytes() != DefaultImportMaxBytes {
		t.Fatal("defaults")
	}
	t.Setenv(EnvImportDedupeWindow, "45s")
	t.Setenv(EnvImportMaxBytes, "2048")
	if ImportDedupeWindow() != 45*time.Second || ImportMaxBytes() != 2048 {
		t.Fatal("overrides")
	}
	for _, bad := range []string{"0", "-1s", "2h", "abc"} {
		t.Setenv(EnvImportDedupeWindow, bad)
		if ImportDedupeWindow() != DefaultImportDedupeWindow {
			t.Fatalf("%q should fall back", bad)
		}
	}
	t.Setenv(EnvImportMaxBytes, "-5")
	if ImportMaxBytes() != DefaultImportMaxBytes {
		t.Fatal("bad max bytes")
	}
}

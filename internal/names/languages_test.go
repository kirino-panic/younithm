package names

import (
	"reflect"
	"testing"
)

func TestCleanLanguages(t *testing.T) {
	got, err := CleanLanguages([]string{" JA ", "en", "ja", "zh-Hant", "", "ko"})
	if err != nil || !reflect.DeepEqual(got, []string{"ja", "en", "zh-Hant", "ko"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, _ := CleanLanguages(nil); !reflect.DeepEqual(got, []string{"en"}) {
		t.Fatalf("empty = %v, want [en]", got)
	}
	if !subset([]string{"ja", "en"}, []string{"en", "ja", "ko"}) || subset([]string{"ko"}, []string{"en"}) {
		t.Fatal("subset")
	}
	for _, bad := range []string{"j", "japanese!", "ja_JP", "1a"} {
		if _, err := CleanLanguages([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := CleanLanguages([]string{"aa", "bb", "cc", "dd", "ee", "ff"}); err == nil {
		t.Error("six languages accepted")
	}
}

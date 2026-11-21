package config

import "testing"

func TestParseBasePath(t *testing.T) {
	ok := map[string]string{
		"": "", "/": "", "  ": "", "younithm": "/younithm", "/younithm/": "/younithm",
		"/a/b": "/a/b", "/my.app~1_x-y": "/my.app~1_x-y",
	}
	for in, want := range ok {
		got, err := ParseBasePath(in)
		if err != nil || got != want {
			t.Errorf("ParseBasePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/a//b", "/../x", "/a/./b", `/"><script>`, "/a b", "/a?b", "/é"} {
		if got, err := ParseBasePath(in); err == nil {
			t.Errorf("ParseBasePath(%q) = %q, want error", in, got)
		}
	}
}

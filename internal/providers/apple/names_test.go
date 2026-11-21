package apple

import "testing"

func TestPickStorefront(t *testing.T) {
	fronts := []storefront{
		{"gb", "en-GB", []string{"en-GB"}},
		{"us", "en-US", []string{"en-US", "es-MX", "ko", "zh-Hant-TW"}},
		{"jp", "ja", []string{"ja", "en-US"}},
	}
	for _, c := range []struct{ lang, sf, tag string }{
		{"en", "us", "en-US"}, // home storefront supports it
		{"ko", "us", "ko"},    // US has Korean
		{"zh-Hant", "us", "zh-Hant-TW"},
		{"ja", "jp", "ja"}, // only JP has Japanese
		{"en-GB", "gb", "en-GB"},
	} {
		sf, tag, ok := pickStorefront(fronts, "us", c.lang)
		if !ok || sf != c.sf || tag != c.tag {
			t.Errorf("%s → %s %s %v, want %s %s", c.lang, sf, tag, ok, c.sf, c.tag)
		}
	}
	if _, _, ok := pickStorefront(fronts, "us", "fi"); ok {
		t.Error("fi: no storefront offers it")
	}
}

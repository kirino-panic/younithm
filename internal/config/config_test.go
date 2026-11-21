package config

import "testing"

func TestParseProxies(t *testing.T) {
	p, err := ParseProxies(" 10.0.0.0/8, 192.168.1.5 ,::1,")
	if err != nil || len(p) != 3 {
		t.Fatalf("%v %v", p, err)
	}
	if p[1].String() != "192.168.1.5/32" || p[2].String() != "::1/128" {
		t.Fatal(p)
	}
	if _, err := ParseProxies("10.0.0.0/8,nope"); err == nil {
		t.Fatal("bad entry accepted")
	}
	if p, err := ParseProxies(""); err != nil || p != nil {
		t.Fatal("empty should be nil")
	}
}

func TestLoadAndValidate(t *testing.T) {
	t.Setenv(EnvTrustedProxies, "172.18.0.0/16")
	t.Setenv(EnvMusicBrainzContact, " me@example.com ")
	c := Load()
	if c.MusicBrainzContact != "me@example.com" || c.TrustedProxies == "" || c.TrustAllProxies {
		t.Fatalf("%+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvTrustedProxyAll, "true")
	if !Load().TrustAllProxies {
		t.Fatal("flag")
	}
}

// The keyless iTunes fallback sends artist and title text to Apple, so it is
// never on unless asked for.
func TestITunesFallbackIsOffByDefault(t *testing.T) {
	t.Setenv(EnvEnableITunes, "")
	if Load().EnableITunes {
		t.Fatal("iTunes fallback is on by default")
	}
	t.Setenv(EnvEnableITunes, "1")
	if !Load().EnableITunes {
		t.Fatal("YOUNITHM_ENABLE_ITUNES=1 did not turn it on")
	}
}

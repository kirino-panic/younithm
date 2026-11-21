package providers

import "testing"

func TestNormalizeBarcode(t *testing.T) {
	for in, want := range map[string]string{
		"199891213535":      "0199891213535", // UPC-A
		"0199891213535":     "0199891213535", // the same release as an EAN-13
		"00602468077992":    "0602468077992", // Spotify pads to 14 digits
		"602468077992":      "0602468077992",
		"4547366570861":     "4547366570861", // a real EAN-13
		" 4547-3665 70861 ": "4547366570861",
		"123":               "",
		"":                  "",
		"abc":               "",
		"12345678901234":    "", // 14 significant digits is not a barcode
	} {
		if got := NormalizeBarcode(in); got != want {
			t.Fatalf("%q: %q want %q", in, got, want)
		}
	}
	if NormalizeBarcode("199891213535") != NormalizeBarcode("0199891213535") {
		t.Fatal("UPC-A and its EAN-13 form must be equal")
	}
}

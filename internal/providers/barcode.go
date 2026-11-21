package providers

import "strings"

// NormalizeBarcode returns the canonical form of a barcode (UPC-A, EAN-13 or a
// zero-padded GTIN) so the same release compares equal across catalogues: its
// digits with leading zeros removed, then left-padded to 13. "" when it is not
// a barcode (fewer than 8 or more than 13 significant digits).
func NormalizeBarcode(s string) string {
	var digits strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	t := strings.TrimLeft(digits.String(), "0")
	if len(t) < 8 || len(t) > 13 {
		return ""
	}
	return strings.Repeat("0", 13-len(t)) + t
}

package enrich

import "younithm/internal/providers"

// ParseTrackURL reads a track link a person pasted (see providers.ParseURL).
func ParseTrackURL(raw string) (provider, id string, ok bool) {
	return providers.ParseURL("recording", raw)
}

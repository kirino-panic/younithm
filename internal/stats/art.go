package stats

import (
	"context"

	"younithm/internal/artwork"
)

// Image URLs for the lists and pages. Each returns "" when there is no image,
// which the response then leaves out.

func (h *Handler) releaseImage(ctx context.Context, id int64) string {
	var p *string
	if h.read.QueryRowContext(ctx, `SELECT `+artwork.ReleaseImage("release")+` FROM release WHERE id = ?`, id).Scan(&p) != nil || p == nil {
		return ""
	}
	return artwork.URL(artwork.KindRelease, id, *p)
}

func (h *Handler) artistImage(ctx context.Context, id int64) string {
	var p *string
	if h.read.QueryRowContext(ctx, `SELECT `+artwork.ArtistImage("artist")+` FROM artist WHERE id = ?`, id).Scan(&p) != nil || p == nil {
		return ""
	}
	return artwork.URL(artwork.KindArtist, id, *p)
}

// recordingImage is the cover of the track's primary album, or of any album it
// is on that has one.
func (h *Handler) recordingImage(ctx context.Context, id int64) string {
	var rid int64
	var p string
	if h.read.QueryRowContext(ctx, `SELECT rel.id, `+artwork.ReleaseImage("rel")+` FROM recording_release rr
JOIN release rel ON rel.id = rr.release_id
WHERE rr.recording_id = ? AND COALESCE(`+artwork.ReleaseImage("rel")+`, '') <> ''
ORDER BY rr.is_primary DESC, rr.release_id LIMIT 1`, id).Scan(&rid, &p) != nil {
		return ""
	}
	return artwork.URL(artwork.KindRelease, rid, p)
}

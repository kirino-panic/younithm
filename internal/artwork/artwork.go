// Package artwork downloads album covers and artist photos from the providers
// an entity is linked to, keeps them under <config>/artwork, and serves them.
//
// A worker looks for releases and artists that have a provider link but no
// image, most-played first, and asks the linked providers in order. It never
// replaces an image that is already set (an admin's choice stays), and it
// remembers misses so they are not retried on every pass.
package artwork

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/config"
	"younithm/internal/providers"
)

// Entity kinds the worker handles; also the {kind} of the serve route.
const (
	KindRelease = "release"
	KindArtist  = "artist"
)

const (
	tsLayout = "2006-01-02T15:04:05.000Z"
	// maxBytes caps one download. Covers are well under 1 MB at the sizes asked for.
	maxBytes = 8 << 20
	// wantWidth is the size to prefer: sharp at 148px on a 3x screen.
	wantWidth = 600
)

// sourceOrder is the order providers are asked in. It matches the naming
// default (Spotify, Apple Music, MusicBrainz).
var sourceOrder = []string{providers.Spotify, providers.AppleMusic, providers.ITunes, providers.MusicBrainz}

var extByType = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif",
}

var typeByExt = map[string]string{
	".jpg": "image/jpeg", ".png": "image/png", ".webp": "image/webp", ".gif": "image/gif",
}

// Options tunes a Worker. Zero values pick the defaults.
type Options struct {
	Poll       time.Duration // how often to look for work; default 30s
	Batch      int           // entities per pass; default 25
	MissRetry  time.Duration // retry after "no image anywhere"; default 14d
	ErrorRetry time.Duration // retry after a failed call or download; default 1h
	Client     *http.Client  // image downloads; default: guarded (https only, public addresses)
	Now        func() time.Time
}

// Worker is the artwork queue. Write must be the write pool.
type Worker struct {
	Read, Write *sql.DB
	Dir         string
	Registry    *providers.Registry
	o           Options
	wake        chan struct{}
	running     atomic.Bool
}

func New(read, write *sql.DB, dir string, reg *providers.Registry, o Options) *Worker {
	if o.Poll <= 0 {
		o.Poll = 30 * time.Second
	}
	if o.Batch <= 0 {
		o.Batch = 25
	}
	if o.MissRetry <= 0 {
		o.MissRetry = 14 * 24 * time.Hour
	}
	if o.ErrorRetry <= 0 {
		o.ErrorRetry = time.Hour
	}
	if o.Client == nil {
		o.Client = guardedClient()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Worker{Read: read, Write: write, Dir: dir, Registry: reg, o: o, wake: make(chan struct{}, 1)}
}

// Wake makes Run look for work now.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run works until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.o.Poll)
	defer t.Stop()
	for {
		for ctx.Err() == nil {
			n, err := w.Pass(ctx)
			if err != nil && ctx.Err() == nil {
				log.Printf("artwork: %v", err)
			}
			if n == 0 || err != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.wake:
		}
	}
}

type target struct {
	kind string
	id   int64
}

// Pass handles one batch of due entities (releases first, then artists) and
// returns how many it looked at. A provider asking us to back off ends the
// pass early with its error.
func (w *Worker) Pass(ctx context.Context) (int, error) {
	if !w.running.CompareAndSwap(false, true) {
		return 0, nil
	}
	defer w.running.Store(false)
	now := w.o.Now()
	var todo []target
	for _, k := range []string{KindRelease, KindArtist} {
		ids, err := w.due(ctx, k, now)
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			todo = append(todo, target{k, id})
		}
	}
	n := 0
	for _, t := range todo {
		if ctx.Err() != nil {
			return n, nil
		}
		outcome, err := w.fetch(ctx, t.kind, t.id)
		var rl *providers.RateLimitedError
		if errors.As(err, &rl) {
			return n, err // try again on the next pass; nothing recorded
		}
		if ctx.Err() != nil {
			return n, nil
		}
		if err := w.record(ctx, t, outcome, now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// due returns up to Batch entity ids of a kind that have an accepted provider
// link and no image, and were not tried recently, most-played first.
func (w *Worker) due(ctx context.Context, kind string, now time.Time) ([]int64, error) {
	table, col, plays := "release", "artwork_path", `(SELECT COUNT(*) FROM scrobble s WHERE s.release_id = e.id)`
	if kind == KindArtist {
		table, col = "artist", "image_path"
		plays = `(SELECT COUNT(*) FROM scrobble s JOIN recording_artist ra ON ra.recording_id = s.recording_id WHERE ra.artist_id = e.id)`
	}
	miss := now.Add(-w.o.MissRetry).UTC().Format(tsLayout)
	fail := now.Add(-w.o.ErrorRetry).UTC().Format(tsLayout)
	rows, err := w.Read.QueryContext(ctx, `
SELECT e.id FROM `+table+` e
WHERE COALESCE(e.`+col+`, '') = ''
  AND EXISTS (SELECT 1 FROM external_link l WHERE l.entity_type = ?1 AND l.entity_id = e.id
      AND l.status IN ('confirmed', 'auto') AND l.match_method <> 'mapping')
  AND NOT EXISTS (SELECT 1 FROM artwork_try t WHERE t.entity_type = ?1 AND t.entity_id = e.id
      AND t.tried_at > CASE t.outcome WHEN 'error' THEN ?3 ELSE ?2 END)
ORDER BY `+plays+` DESC, e.id LIMIT ?4`, kind, miss, fail, w.o.Batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (w *Worker) record(ctx context.Context, t target, outcome string, now time.Time) error {
	_, err := w.Write.ExecContext(ctx, `
INSERT INTO artwork_try (entity_type, entity_id, outcome, tried_at) VALUES (?1, ?2, ?3, ?4)
ON CONFLICT(entity_type, entity_id) DO UPDATE SET outcome = ?3, tried_at = ?4`,
		t.kind, t.id, outcome, now.UTC().Format(tsLayout))
	return err
}

type link struct{ provider, id string }

// links returns an entity's accepted provider ids in the order providers are asked.
func (w *Worker) links(ctx context.Context, kind string, id int64) ([]link, error) {
	rows, err := w.Read.QueryContext(ctx, `SELECT provider, provider_id FROM external_link
WHERE entity_type = ? AND entity_id = ? AND status IN ('confirmed', 'auto') AND match_method <> 'mapping'
ORDER BY status = 'confirmed' DESC, pinned DESC, confidence DESC, id`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string][]link{}
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.provider, &l.id); err != nil {
			return nil, err
		}
		by[l.provider] = append(by[l.provider], l)
	}
	var out []link
	for _, p := range sourceOrder {
		out = append(out, by[p]...)
	}
	return out, rows.Err()
}

// fetch tries each linked provider until one yields an image that downloads,
// and stores it. outcome is "found", "none" or "error".
func (w *Worker) fetch(ctx context.Context, kind string, id int64) (string, error) {
	ls, err := w.links(ctx, kind, id)
	if err != nil {
		return "error", err
	}
	failed := false
	for _, l := range ls {
		p, ok := w.Registry.Get(l.provider)
		if !ok {
			continue
		}
		var refs []providers.ImageRef
		if kind == KindRelease {
			refs, err = p.GetArtwork(ctx, l.id)
		} else {
			refs, err = p.GetArtistImage(ctx, l.id)
		}
		switch {
		case err == nil:
		case errors.Is(err, providers.ErrNotFound), errors.Is(err, providers.ErrUnsupported), errors.Is(err, providers.ErrNotConfigured):
			continue
		default:
			var rl *providers.RateLimitedError
			if errors.As(err, &rl) {
				return "error", err
			}
			failed = true
			continue
		}
		ref, ok := pick(refs)
		if !ok {
			continue
		}
		data, ext, err := w.download(ctx, ref.URL)
		if err != nil {
			log.Printf("artwork: %s %d from %s: %v", kind, id, l.provider, err)
			failed = true
			continue
		}
		// Not stored means the entity got an image meanwhile: nothing left to do.
		if _, err := w.store(ctx, kind, id, data, ext); err != nil {
			return "error", err
		}
		return "found", nil
	}
	if failed {
		return "error", nil
	}
	return "none", nil
}

// pick chooses the image to download: the smallest at least wantWidth wide,
// else the widest. Refs without a size (the first is the best) rank as given.
func pick(refs []providers.ImageRef) (providers.ImageRef, bool) {
	var best providers.ImageRef
	found := false
	for _, r := range refs {
		if r.URL == "" || (r.Kind != "" && r.Kind != "front" && r.Kind != "artist") {
			continue
		}
		if !found {
			best, found = r, true
			continue
		}
		if r.Width == 0 || best.Width == 0 {
			continue
		}
		switch {
		case best.Width < wantWidth && r.Width > best.Width:
			best = r
		case best.Width >= wantWidth && r.Width >= wantWidth && r.Width < best.Width:
			best = r
		}
	}
	return best, found
}

// store writes the image and points the entity at it, unless the entity got an
// image in the meantime. It reports whether it set one.
func (w *Worker) store(ctx context.Context, kind string, id int64, data []byte, ext string) (bool, error) {
	sum := sha256.Sum256(data)
	name := fmt.Sprintf("%s-%d-%s%s", kind, id, hex.EncodeToString(sum[:4]), ext)
	if err := os.MkdirAll(w.Dir, 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(w.Dir, ".dl-*")
	if err != nil {
		return false, err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return false, errors.Join(werr, cerr)
	}
	dst := filepath.Join(w.Dir, name)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return false, err
	}
	table, col := "release", "artwork_path"
	if kind == KindArtist {
		table, col = "artist", "image_path"
	}
	res, err := w.Write.ExecContext(ctx, `UPDATE `+table+` SET `+col+` = ?1 WHERE id = ?2 AND COALESCE(`+col+`, '') = ''`, name, id)
	if err != nil {
		os.Remove(dst)
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		os.Remove(dst)
		return false, nil
	}
	return true, nil
}

// download fetches one image, checking its type by content, not by header.
func (w *Worker) download(ctx context.Context, raw string) ([]byte, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, "", fmt.Errorf("bad image url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "younithm (artwork cache)")
	resp, err := w.o.Client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxBytes {
		return nil, "", fmt.Errorf("image larger than %d bytes", maxBytes)
	}
	ext, ok := extByType[http.DetectContentType(data)]
	if !ok {
		return nil, "", fmt.Errorf("not an image")
	}
	return data, ext, nil
}

// guardedClient only follows https and only connects to public addresses, so a
// provider answer cannot point the server at its own network.
func guardedClient() *http.Client {
	d := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
				ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
				return fmt.Errorf("refusing non-public address %s", host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" {
				return errors.New("redirect to a non-https url")
			}
			return nil
		},
	}
}

// ReleaseImage is the SQL for the image a release shows: one chosen by hand,
// else the fetched one. alias names the release table in the query.
func ReleaseImage(alias string) string {
	return `COALESCE(NULLIF(` + alias + `.image_override, ''), ` + alias + `.artwork_path)`
}

// ArtistImage is ReleaseImage for an artist.
func ArtistImage(alias string) string {
	return `COALESCE(NULLIF(` + alias + `.image_override, ''), ` + alias + `.image_path)`
}

// SaveUpload checks an uploaded image by its content and writes it under dir.
// It returns the file name to store; the entity is not changed.
func SaveUpload(dir, kind string, id int64, data []byte) (string, error) {
	if kind != KindRelease && kind != KindArtist {
		return "", fmt.Errorf("images are for albums and artists")
	}
	if len(data) > maxBytes {
		return "", ErrTooLarge
	}
	ext, ok := extByType[http.DetectContentType(data)]
	if !ok {
		return "", ErrNotImage
	}
	sum := sha256.Sum256(data)
	name := fmt.Sprintf("%s-%d-u%s%s", kind, id, hex.EncodeToString(sum[:4]), ext)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".up-*")
	if err != nil {
		return "", err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return "", errors.Join(werr, cerr)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return name, nil
}

// Upload errors a person can act on.
var (
	ErrTooLarge = fmt.Errorf("image larger than %d MB", maxBytes>>20)
	ErrNotImage = errors.New("not a JPEG, PNG, WebP or GIF image")
)

// MaxUpload is the largest image SaveUpload takes.
const MaxUpload = maxBytes

// ---- serving ----

// URL is where the page loads an entity's image from, or "" when it has none.
// The file name carries a content hash, which becomes ?v= so browsers can keep
// the image forever and still pick up a replacement.
func URL(kind string, id int64, path string) string {
	if path == "" {
		return ""
	}
	u := config.BasePath() + "/api/v1/art/" + kind + "/" + strconv.FormatInt(id, 10)
	stem := strings.TrimSuffix(path, filepath.Ext(path))
	if i := strings.LastIndex(stem, "-"); i >= 0 && i < len(stem)-1 {
		u += "?v=" + url.QueryEscape(stem[i+1:])
	}
	return u
}

// Handler serves stored images at GET /api/v1/art/{kind}/{id}. It is public and
// unlimited on purpose: a page shows many covers at once, and an image says
// nothing the page itself does not.
type Handler struct {
	Read *sql.DB
	Dir  string
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kind, rawID := chi.URLParam(r, "kind"), chi.URLParam(r, "id")
	var q string
	switch kind {
	case KindRelease:
		q = `SELECT ` + ReleaseImage("release") + ` FROM release WHERE id = ?`
	case KindArtist:
		q = `SELECT ` + ArtistImage("artist") + ` FROM artist WHERE id = ?`
	default:
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var path sql.NullString
	if err := h.Read.QueryRowContext(r.Context(), q, id).Scan(&path); err != nil || path.String == "" {
		http.NotFound(w, r)
		return
	}
	name := path.String
	if name != filepath.Base(name) || strings.HasPrefix(name, ".") { // never leave the folder
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(h.Dir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	ct, ok := typeByExt[strings.ToLower(filepath.Ext(name))]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(w, r, name, st.ModTime(), f)
}

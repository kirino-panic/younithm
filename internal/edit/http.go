package edit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"younithm/internal/providers"

	"github.com/go-chi/chi/v5"

	"younithm/internal/artwork"
	"younithm/internal/auth"
	"younithm/internal/db"
)

// Mount registers the admin editing API. Everything needs an admin session
// (which also enforces CSRF on writes). Edit history and raw values are
// admin-only.
//
//	GET    /api/v1/admin/edits?entity_type&entity_id&scope&before_id&limit
//	GET    /api/v1/admin/edits/{id}                      full before/after
//	POST   /api/v1/admin/edits/{id}/undo
//	GET    /api/v1/admin/scrobbles/{id}                  raw vs effective
//	PATCH  /api/v1/admin/scrobbles/{id}                  {played_at, recording_id, release_id, link_release}
//	DELETE /api/v1/admin/scrobbles/{id}
//	POST   /api/v1/admin/scrobbles/delete                {ids}  one edit, one undo
//	POST   /api/v1/admin/scrobbles/{id}/move-album       {release_id, link_release}
//	POST   /api/v1/admin/scrobbles/bulk/preview          {filter, set}
//	POST   /api/v1/admin/scrobbles/bulk/apply            {filter, set, expected_matched}
//	PATCH  /api/v1/admin/entities/{kind}/{id}            {fields, drop_alias}
//	POST   /api/v1/admin/entities/{kind}/{id}/merge      {into}
//	GET    /api/v1/admin/entities/{kind}/{id}/merge-candidates?q   suggestions when q is empty
//	GET    /api/v1/admin/entities/{kind}/candidates?q    the same search, for a scrobble with no track
//	POST   /api/v1/admin/recordings/{id}/releases        {release_id, disc, position}
//	DELETE /api/v1/admin/recordings/{id}/releases/{rid}
//	PUT    /api/v1/admin/recordings/{id}/primary-release {release_id}
//	POST   /api/v1/admin/entities/{kind}/{id}/image      multipart "file"   (artist, release)
//	DELETE /api/v1/admin/entities/{kind}/{id}/image      back to the fetched image
//
// Hooks lets the server react to edits other parts care about.
type Hooks struct {
	// LinkChanged runs after a catalogue link was set or removed by hand.
	LinkChanged func(kind string, id int64)
	// NamesChanged runs after an edit that can change which name shows
	// (a rename, a pin cleared, a merge, an undo).
	NamesChanged func()
	// ArtDir is the artwork folder images chosen by hand are saved in.
	ArtDir string
}

func (h *handlers) namesChanged(err error) {
	if err == nil && h.hooks.NamesChanged != nil {
		h.hooks.NamesChanged()
	}
}

func Mount(r chi.Router, pools *db.Pools, hooks ...Hooks) {
	h := &handlers{ed: &Editor{DB: pools.Write}, read: pools.Read}
	if len(hooks) > 0 {
		h.hooks = hooks[0]
	}
	const p = "/api/v1/admin"
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireSession)
		r.Get(p+"/edits", h.list)
		r.Get(p+"/edits/{id}", h.get)
		r.Post(p+"/edits/{id}/undo", h.undo)
		r.Get(p+"/scrobbles/{id}", h.view)
		r.Patch(p+"/scrobbles/{id}", h.editScrobble)
		r.Delete(p+"/scrobbles/{id}", h.deleteScrobble)
		r.Post(p+"/scrobbles/delete", h.deleteScrobbles)
		r.Post(p+"/scrobbles/{id}/move-album", h.moveAlbum)
		r.Post(p+"/scrobbles/bulk/preview", h.bulkPreview)
		r.Patch(p+"/entities/{kind}/{id}", h.editEntity)
		r.Post(p+"/recordings/{id}/releases", h.addRelease)
		r.Delete(p+"/recordings/{id}/releases/{rid}", h.removeRelease)
		r.Put(p+"/recordings/{id}/primary-release", h.setPrimary)
		r.Put(p+"/entities/{kind}/{id}/artists", h.setCredits)
		r.Put(p+"/entities/{kind}/{id}/links", h.setLink)
		r.Post(p+"/entities/{kind}/{id}/image", h.setImage)
		r.Delete(p+"/entities/{kind}/{id}/image", h.resetImage)
		r.Delete(p+"/entities/{kind}/{id}/links/{provider}", h.removeLink)
		r.Get(p+"/search", h.search)
		r.Get(p+"/entities/{kind}/{id}/merge-candidates", h.mergeCandidates)
		r.Get(p+"/entities/{kind}/candidates", h.candidates)
		r.Post(p+"/scrobbles/bulk/apply", h.bulkApply)
		r.Post(p+"/entities/{kind}/{id}/merge", h.merge)
		r.Post(p+"/artists/{id}/split", h.split)
		r.Delete(p+"/entities/{kind}/{id}", h.deleteEntity)
	})
}

type handlers struct {
	ed    *Editor
	read  Querier
	hooks Hooks
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	var e *Error
	if errors.As(err, &e) {
		body := map[string]any{"error": e.Code, "message": e.Msg}
		if len(e.Conflicts) > 0 {
			body["conflicts"] = e.Conflicts
		}
		writeJSON(w, e.Status, body)
		return
	}
	log.Printf("edit: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal"})
}

func actor(r *http.Request) string {
	if p := auth.PrincipalFrom(r.Context()); p != nil {
		return fmt.Sprintf("admin:%d", p.UserID)
	}
	return "admin"
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		fail(w, invalid("bad_request", "invalid JSON body"))
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id <= 0 {
		fail(w, invalid("bad_id", "invalid %s", name))
		return 0, false
	}
	return id, true
}

func done(w http.ResponseWriter, res *Result, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{EntityType: q.Get("entity_type"), Scope: q.Get("scope")}
	f.EntityID, _ = strconv.ParseInt(q.Get("entity_id"), 10, 64)
	f.BeforeID, _ = strconv.ParseInt(q.Get("before_id"), 10, 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	items, err := List(r.Context(), h.read, f)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	en, err := Get(r.Context(), h.read, id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, en)
}

func (h *handlers) undo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	res, err := h.ed.Undo(r.Context(), actor(r), id)
	h.namesChanged(err)
	done(w, res, err)
}

func (h *handlers) view(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	v, err := View(r.Context(), h.read, id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *handlers) editScrobble(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var ed ScrobbleEdit
	if !decode(w, r, &ed) {
		return
	}
	res, err := h.ed.EditScrobble(r.Context(), actor(r), id, ed)
	done(w, res, err)
}

func (h *handlers) deleteScrobble(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	res, err := h.ed.DeleteScrobble(r.Context(), actor(r), id)
	done(w, res, err)
}

func (h *handlers) deleteScrobbles(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := h.ed.DeleteScrobbles(r.Context(), actor(r), in.IDs)
	done(w, res, err)
}

func (h *handlers) moveAlbum(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		ReleaseID   int64 `json:"release_id"`
		LinkRelease bool  `json:"link_release"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.ReleaseID <= 0 {
		fail(w, invalid("bad_request", "release_id required"))
		return
	}
	res, err := h.ed.MoveScrobbleToRelease(r.Context(), actor(r), id, in.ReleaseID, in.LinkRelease)
	done(w, res, err)
}

type bulkBody struct {
	Filter          Filter       `json:"filter"`
	Set             ScrobbleEdit `json:"set"`
	ExpectedMatched *int         `json:"expected_matched"`
}

func (h *handlers) bulkPreview(w http.ResponseWriter, r *http.Request) {
	var in bulkBody
	if !decode(w, r, &in) {
		return
	}
	pv, err := h.ed.BulkDryRun(r.Context(), in.Filter, in.Set)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

func (h *handlers) bulkApply(w http.ResponseWriter, r *http.Request) {
	var in bulkBody
	if !decode(w, r, &in) {
		return
	}
	if in.ExpectedMatched == nil {
		fail(w, invalid("preview_required", "run bulk/preview first and pass its matched count as expected_matched"))
		return
	}
	res, err := h.ed.BulkApply(r.Context(), actor(r), in.Filter, in.Set, *in.ExpectedMatched)
	done(w, res, err)
}

func (h *handlers) editEntity(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Fields    map[string]any `json:"fields"`
		DropAlias bool           `json:"drop_alias"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := h.ed.EditEntity(r.Context(), actor(r), chi.URLParam(r, "kind"), id, EntityEdit{Fields: in.Fields, DropAlias: in.DropAlias})
	h.namesChanged(err)
	done(w, res, err)
}

func (h *handlers) merge(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Into int64 `json:"into"`
		Once bool  `json:"once"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := h.ed.MergeWith(r.Context(), actor(r), chi.URLParam(r, "kind"), id, in.Into, MergeOptions{Once: in.Once})
	h.namesChanged(err)
	done(w, res, err)
}

func (h *handlers) deleteEntity(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	res, err := h.ed.DeleteEntity(r.Context(), actor(r), chi.URLParam(r, "kind"), id)
	h.namesChanged(err)
	done(w, res, err)
}

func (h *handlers) split(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Artists []CreditIn `json:"artists"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := h.ed.Split(r.Context(), actor(r), id, in.Artists)
	h.namesChanged(err)
	done(w, res, err)
}

func (h *handlers) addRelease(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		ReleaseID int64  `json:"release_id"`
		Title     string `json:"title"` // no release_id: a new album with this title
		Disc      int64  `json:"disc"`
		Position  *int64 `json:"position"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.ReleaseID == 0 && in.Title != "" {
		res, err := h.ed.AddToNewRelease(r.Context(), actor(r), id, in.Title)
		done(w, res, err)
		return
	}
	res, err := h.ed.AddToRelease(r.Context(), actor(r), id, in.ReleaseID, in.Disc, in.Position)
	done(w, res, err)
}

func (h *handlers) removeRelease(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	rid, ok := pathID(w, r, "rid")
	if !ok {
		return
	}
	res, err := h.ed.RemoveFromRelease(r.Context(), actor(r), id, rid)
	done(w, res, err)
}

func (h *handlers) setPrimary(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		ReleaseID int64 `json:"release_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := h.ed.SetPrimaryRelease(r.Context(), actor(r), id, in.ReleaseID)
	done(w, res, err)
}

func (h *handlers) setCredits(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Artists []CreditIn `json:"artists"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := h.ed.SetCredits(r.Context(), actor(r), chi.URLParam(r, "kind"), id, in.Artists)
	done(w, res, err)
}

func (h *handlers) linkChanged(kind string, id int64) {
	if h.hooks.LinkChanged != nil {
		h.hooks.LinkChanged(kind, id)
	}
}

func (h *handlers) setLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	kind := chi.URLParam(r, "kind")
	var in struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &in) {
		return
	}
	provider, pid, parsed := providers.ParseURL(kind, in.URL)
	if !parsed {
		fail(w, invalid("bad_url", "not a MusicBrainz, Spotify or Apple Music link for this %s", kindLabel(kind)))
		return
	}
	res, err := h.ed.SetLink(r.Context(), actor(r), kind, id, provider, pid)
	if err == nil {
		h.linkChanged(kind, id)
	}
	done(w, res, err)
}

func (h *handlers) removeLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	kind := chi.URLParam(r, "kind")
	res, err := h.ed.RemoveLink(r.Context(), actor(r), kind, id, chi.URLParam(r, "provider"))
	if err == nil {
		h.linkChanged(kind, id)
	}
	done(w, res, err)
}

func (h *handlers) search(w http.ResponseWriter, r *http.Request) {
	hits, err := Search(r.Context(), h.read, r.URL.Query().Get("kind"), r.URL.Query().Get("q"), 8)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": hits})
}

func kindLabel(kind string) string {
	return map[string]string{KindRecording: "track", KindRelease: "album", KindArtist: "artist"}[kind]
}

func (h *handlers) setImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	kind := chi.URLParam(r, "kind")
	if kind != KindArtist && kind != KindRelease {
		fail(w, invalid("bad_kind", "images can be set for artists and albums"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, artwork.MaxUpload+1<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		fail(w, invalid("bad_file", "choose an image up to %d MB", artwork.MaxUpload>>20))
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, artwork.MaxUpload+1))
	if err != nil {
		fail(w, invalid("bad_file", "couldn't read the image"))
		return
	}
	if _, err := h.ed.entityExists(r.Context(), h.read, kind, id); err != nil {
		fail(w, err)
		return
	}
	name, err := artwork.SaveUpload(h.hooks.ArtDir, kind, id, data)
	if errors.Is(err, artwork.ErrTooLarge) || errors.Is(err, artwork.ErrNotImage) {
		fail(w, invalid("bad_file", "%s", err.Error()))
		return
	}
	if err != nil {
		log.Printf("edit: save image: %v", err)
		fail(w, err)
		return
	}
	res, err := h.ed.SetImage(r.Context(), actor(r), kind, id, name)
	done(w, res, err)
}

func (h *handlers) resetImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	res, err := h.ed.SetImage(r.Context(), actor(r), chi.URLParam(r, "kind"), id, "")
	done(w, res, err)
}

// candidates searches entities of a kind by name with nothing excluded, for
// re-linking a scrobble that has no track yet.
func (h *handlers) candidates(w http.ResponseWriter, r *http.Request) {
	c, err := MergeSearch(r.Context(), h.read, chi.URLParam(r, "kind"), 0, r.URL.Query().Get("q"), 50)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": c})
}

func (h *handlers) mergeCandidates(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	kind := chi.URLParam(r, "kind")
	var c []Candidate
	var err error
	if q := r.URL.Query().Get("q"); q != "" {
		c, err = MergeSearch(r.Context(), h.read, kind, id, q, 50)
	} else {
		c, err = MergeSuggestions(r.Context(), h.read, kind, id, 20)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": c})
}

package imports

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"younithm/internal/auth"
	"younithm/internal/importers"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

// Mount registers the admin import endpoints under /admin/imports of r
// (normally the /api/v1 group). Everything needs an admin session.
//
//	POST   /admin/imports              multipart "file"; ?format= ?dedupe_window= ?spotify_min_ms= ?fuzzy_consent= ?debug_limit=
//	POST   /admin/imports/check        {"head": base64} or {"entries": [...]}: format (and Apple file roles), nothing stored
//	POST   /admin/imports/{id}/files   another file for an import that is awaiting_files (Apple Music: both CSVs)
//	GET    /admin/imports              jobs, supported formats, defaults
//	GET    /admin/imports/{id}         job with preview/result
//	POST   /admin/imports/{id}/confirm apply the previewed import; ?when_ready=1 also
//	                                accepts a job still awaiting files or being read: it applies once previewed
//	POST   /admin/imports/{id}/retry   re-run a failed job
//	POST   /admin/imports/{id}/cancel
//	DELETE /admin/imports/{id}         remove a finished job record
func (m *Manager) Mount(r chi.Router) { m.MountWith(r, auth.RequireSession) }

// MountWith is Mount with explicit middleware (tests).
func (m *Manager) MountWith(r chi.Router, session func(http.Handler) http.Handler) {
	r.Route("/admin/imports", func(r chi.Router) {
		r.Use(session)
		r.Post("/", m.upload)
		r.Post("/check", m.checkFile)
		r.Get("/", m.list)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", m.get)
			r.Post("/cancel", m.cancel)
			r.Delete("/", m.delete)
			r.Post("/confirm", m.confirm)
			r.Post("/retry", m.retry)
			r.Post("/files", m.addFile)
		})
	})
}

func (m *Manager) list(w http.ResponseWriter, r *http.Request) {
	jobs, err := m.store.List(r.Context(), 100)
	if err != nil {
		apiErr(w, 500, "internal", "listing jobs failed")
		return
	}
	for i := range jobs { // keep the list light; details have the rest
		jobs[i].Preview, jobs[i].Result = nil, nil
	}
	if jobs == nil {
		jobs = []Job{}
	}
	writeJSON(w, 200, map[string]any{
		"jobs":    jobs,
		"formats": FormatNames(),
		// Formats that need several files, and which: nothing runs on such an
		// import until every one has been uploaded (POST /admin/imports/{id}/files).
		"required_files": RequiredFiles(),
		"defaults": map[string]any{
			"dedupe_window_sec":      m.EffectiveWindow(0).Seconds(),
			"live_dedupe_window_sec": m.opt.LiveWindow.Seconds(),
			"max_upload_bytes":       m.opt.MaxBytes,
		},
	})
}

func jobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		apiErr(w, 404, "not_found", "no such import")
		return 0, false
	}
	return id, true
}

func (m *Manager) get(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	j, err := m.store.Get(r.Context(), id)
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, 200, j)
}

func (m *Manager) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		apiErr(w, 404, "not_found", "no such import")
	case errors.Is(err, ErrState):
		apiErr(w, 409, "invalid_state", err.Error())
	default:
		apiErr(w, 500, "internal", "request failed")
	}
}

func (m *Manager) action(w http.ResponseWriter, r *http.Request, fn func(id int64) error) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	if err := fn(id); err != nil {
		m.fail(w, err)
		return
	}
	j, err := m.store.Get(r.Context(), id)
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, 200, j)
}

func (m *Manager) confirm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("when_ready") == "1" {
		m.action(w, r, func(id int64) error { return m.ConfirmWhenReady(r.Context(), id) })
		return
	}
	m.action(w, r, func(id int64) error { return m.Confirm(r.Context(), id) })
}
func (m *Manager) retry(w http.ResponseWriter, r *http.Request) {
	m.action(w, r, func(id int64) error { return m.Retry(r.Context(), id) })
}
func (m *Manager) cancel(w http.ResponseWriter, r *http.Request) {
	m.action(w, r, func(id int64) error { return m.Cancel(r.Context(), id) })
}
func (m *Manager) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	if err := m.Delete(r.Context(), id); err != nil {
		m.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil { // bare number = seconds
		if f <= 0 {
			return 0, errors.New("must be positive")
		}
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err == nil && d <= 0 {
		err = errors.New("must be positive")
	}
	return d, err
}

// received is an upload streamed to disk: the temp file, its name, size and
// SHA-256, and the non-file form fields (the query string wins ties).
type received struct {
	tmp, filename string
	size          int64
	sha           string
	fields        map[string]string
}

// cleanup removes the temp file unless it has been moved into place.
func (rc *received) cleanup() {
	if rc.tmp != "" {
		_ = os.Remove(rc.tmp)
	}
}

// receive streams the multipart body to disk (never into memory), hashing it.
// On failure it has already answered the request and returns false.
func (m *Manager) receive(w http.ResponseWriter, r *http.Request) (*received, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, m.opt.MaxBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		apiErr(w, 400, "bad_request", "expected multipart/form-data with a \"file\" part")
		return nil, false
	}
	rc := &received{fields: map[string]string{}}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			rc.fields[k] = v[0]
		}
	}
	if err := os.MkdirAll(m.opt.Dir, 0o700); err != nil {
		apiErr(w, 500, "internal", "cannot create the import directory")
		return nil, false
	}
	h := sha256.New()
	ok := false
	defer func() {
		if !ok {
			rc.cleanup()
		}
	}()
	for rc.tmp == "" {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			m.uploadErr(w, err)
			return nil, false
		}
		if part.FormName() != "file" {
			b, _ := io.ReadAll(io.LimitReader(part, 1024))
			rc.fields[part.FormName()] = string(b)
			continue
		}
		rc.filename = filepath.Base(part.FileName())
		f, err := os.CreateTemp(m.opt.Dir, "upload-*.tmp")
		if err != nil {
			apiErr(w, 500, "internal", "cannot store the upload")
			return nil, false
		}
		rc.tmp = f.Name()
		n, cerr := io.Copy(io.MultiWriter(f, h), io.LimitReader(part, m.opt.MaxBytes+1))
		rc.size = n
		if cerr2 := f.Close(); cerr == nil {
			cerr = cerr2
		}
		if cerr != nil {
			m.uploadErr(w, cerr)
			return nil, false
		}
	}
	if rc.tmp == "" {
		apiErr(w, 400, "bad_request", "no \"file\" part in the upload")
		return nil, false
	}
	if rc.size > m.opt.MaxBytes {
		apiErr(w, 413, "too_large", "file exceeds the upload limit")
		return nil, false
	}
	if rc.size == 0 {
		apiErr(w, 400, "empty_file", "the uploaded file is empty")
		return nil, false
	}
	rc.sha = hex.EncodeToString(h.Sum(nil))
	ok = true
	return rc, true
}

// place moves the received file to its final, unguessable name.
func (m *Manager) place(rc *received) (string, error) {
	var rnd [12]byte
	_, _ = rand.Read(rnd[:])
	final := filepath.Join(m.opt.Dir, hex.EncodeToString(rnd[:])+".upload")
	if err := os.Rename(rc.tmp, final); err != nil {
		return "", err
	}
	rc.tmp = ""
	return final, nil
}

// upload receives a file, detects its format and creates the import job. A format
// that needs several files (Apple Music) creates a job that waits for the rest.
func (m *Manager) upload(w http.ResponseWriter, r *http.Request) {
	rc, ok := m.receive(w, r)
	if !ok {
		return
	}
	defer rc.cleanup()
	fields := rc.fields

	format, detected := strings.ToLower(strings.TrimSpace(fields["format"])), false
	if format == "" || format == "auto" {
		f, err := Detect(rc.tmp)
		if err != nil {
			apiErr(w, 422, "unknown_format", err.Error()+" (supported: "+strings.Join(FormatNames(), ", ")+")")
			return
		}
		format, detected = f, true
	} else if _, ok := importers.Registry()[format]; !ok {
		apiErr(w, 400, "bad_format", "unknown format "+format+" (supported: "+strings.Join(FormatNames(), ", ")+")")
		return
	}
	win, err := parseWindow(fields["dedupe_window"])
	if err != nil {
		apiErr(w, 400, "bad_dedupe_window", "dedupe_window: "+err.Error())
		return
	}
	var opts JobOptions
	opts.DedupeWindowSec = m.EffectiveWindow(win).Seconds()
	if v := fields["spotify_min_ms"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			apiErr(w, 400, "bad_spotify_min_ms", "spotify_min_ms must be an integer")
			return
		}
		opts.SpotifyMinMs = n
	}

	if v := strings.ToLower(strings.TrimSpace(fields["fuzzy_consent"])); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			apiErr(w, 400, "bad_fuzzy_consent", "fuzzy_consent must be true or false")
			return
		}
		opts.FuzzyConsent = b
	}
	if v := strings.TrimSpace(fields["debug_limit"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			apiErr(w, 400, "bad_debug_limit", "debug_limit must be a non-negative integer")
			return
		}
		opts.DebugLimit = n
	}
	if opts.DebugLimit == 0 && m.opt.DebugLimit != nil {
		opts.DebugLimit = m.opt.DebugLimit()
	}

	j := Job{Filename: rc.filename, Format: format, FormatDetected: detected, FileSize: rc.size,
		FileSHA256: rc.sha, Options: opts}
	if requiredFiles(format) != nil {
		m.createMulti(w, r, rc, j)
		return
	}
	// Move the file to its final name first: the worker may claim the job
	// the instant the row exists, so the row must carry the real path.
	final, err := m.place(rc)
	if err != nil {
		apiErr(w, 500, "internal", "cannot store the upload")
		return
	}
	id, err := m.store.Create(r.Context(), j, final)
	if err != nil {
		_ = os.Remove(final)
		apiErr(w, 500, "internal", "cannot create the job")
		return
	}
	m.Wake()
	job, err := m.store.getW(r.Context(), id)
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

// classify returns the file rows an upload provides for a multi-file format, with
// the stored path, size and hash filled in.
func (m *Manager) classify(rc *received, format string) ([]fileRef, error) {
	if format != importers.SourceAppleMusic {
		return nil, fmt.Errorf("%s is not a multi-file format", format)
	}
	rows, err := appleFileRoles(rc.tmp)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].filename == "" {
			rows[i].filename = rc.filename
		}
		rows[i].size, rows[i].sha = rc.size, rc.sha
	}
	return rows, nil
}

// createMulti creates a multi-file job from its first upload. If the upload does
// not hold every required file the job waits for the rest and nothing is scanned,
// joined, previewed or applied until they are all here.
func (m *Manager) createMulti(w http.ResponseWriter, r *http.Request, rc *received, j Job) {
	rows, err := m.classify(rc, j.Format)
	if err != nil {
		apiErr(w, 422, "wrong_file", err.Error())
		return
	}
	final, err := m.place(rc)
	if err != nil {
		apiErr(w, 500, "internal", "cannot store the upload")
		return
	}
	for i := range rows {
		rows[i].path = final
	}
	have := map[string]bool{}
	for _, f := range rows {
		have[f.role] = true
	}
	awaiting := false
	for _, d := range requiredFiles(j.Format) {
		if !have[d.Role] {
			awaiting = true
		}
	}
	id, err := m.store.CreateMulti(r.Context(), j, rows, awaiting)
	if err != nil {
		_ = os.Remove(final)
		apiErr(w, 500, "internal", "cannot create the job")
		return
	}
	if !awaiting {
		m.Wake()
	}
	job, err := m.store.getW(r.Context(), id)
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

// addFile attaches another file to a multi-file job that is waiting for it:
//
//	POST /admin/imports/{id}/files   multipart "file"; ?replace=true swaps a file already there
//
// A file whose role is already filled is refused unless replace is set. The
// moment the last required file is here the job is released to the worker.
func (m *Manager) addFile(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	j, err := m.store.getW(r.Context(), id)
	if err != nil {
		m.fail(w, err)
		return
	}
	if j.Status != StatusAwaitingFiles {
		apiErr(w, 409, "invalid_state", "this import is not waiting for files")
		return
	}
	rc, ok := m.receive(w, r)
	if !ok {
		return
	}
	defer rc.cleanup()
	rows, err := m.classify(rc, j.Format)
	if err != nil {
		apiErr(w, 422, "wrong_file", err.Error())
		return
	}
	replace, _ := strconv.ParseBool(rc.fields["replace"])
	final, err := m.place(rc)
	if err != nil {
		apiErr(w, 500, "internal", "cannot store the upload")
		return
	}
	for i := range rows {
		rows[i].path = final
	}
	orphans, complete, err := m.store.AddFiles(r.Context(), id, rows, replace)
	if err != nil {
		_ = os.Remove(final)
		switch {
		case errors.Is(err, ErrAlreadyPresent):
			apiErr(w, 409, "already_present", err.Error()+": upload with replace=true to swap it")
		default:
			m.fail(w, err)
		}
		return
	}
	for _, p := range orphans {
		_ = os.Remove(p)
	}
	if complete {
		m.Wake()
	}
	job, err := m.store.getW(r.Context(), id)
	if err != nil {
		m.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (m *Manager) uploadErr(w http.ResponseWriter, err error) {
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		apiErr(w, 413, "too_large", "file exceeds the upload limit")
		return
	}
	apiErr(w, 400, "bad_request", "upload failed: "+err.Error())
}

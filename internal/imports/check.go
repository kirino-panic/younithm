package imports

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"younithm/internal/importers"
)

// Checking a file before anything is uploaded: the welcome flow holds imports
// in the browser until the very end, but still has to say right away when a
// file won't work. The browser sends what Detect would read (the first 64 KiB,
// or a zip's entry names) and gets back the format and, for Apple Music, which
// of the two files it is.

type checkReq struct {
	Head    string   `json:"head"`    // base64 of the first 64 KiB (plain files)
	Entries []string `json:"entries"` // entry names (zips)
}

type checkResp struct {
	Format string   `json:"format"`
	Roles  []string `json:"roles,omitempty"` // Apple Music files present
}

// checkFile answers POST /admin/imports/check.
func (m *Manager) checkFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in checkReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		apiErr(w, 400, "bad_request", "expected {\"head\"} or {\"entries\"}")
		return
	}
	var out checkResp
	if in.Entries != nil {
		f, roles, err := checkZipNames(in.Entries)
		if err != nil {
			apiErr(w, 422, "unknown_format", err.Error())
			return
		}
		out = checkResp{Format: f, Roles: roles}
	} else {
		head, err := base64.StdEncoding.DecodeString(in.Head)
		if err != nil || len(head) == 0 {
			apiErr(w, 400, "bad_request", "head must be base64 file contents")
			return
		}
		if len(head) > 64*1024 {
			head = head[:64*1024]
		}
		f, err := sniffText(head)
		if err != nil {
			apiErr(w, 422, "unknown_format", err.Error())
			return
		}
		out.Format = f
		if f == importers.SourceAppleMusic {
			line, _, _ := strings.Cut(strings.TrimPrefix(string(head), "\xef\xbb\xbf"), "\n")
			if role := importers.RoleOf(line); role != "" {
				out.Roles = []string{role}
			}
		}
	}
	writeJSON(w, 200, out)
}

// checkZipNames is Detect's zip branch on names alone. Apple's privacy export
// is classified by entry name here (the import itself reads each header).
func checkZipNames(names []string) (string, []string, error) {
	var roles []string
	seen := map[string]bool{}
	format := ""
	for _, n := range names {
		ln := strings.ToLower(n)
		if strings.HasPrefix(ln, "__macosx/") {
			continue
		}
		switch {
		case strings.HasSuffix(ln, ".jsonl") && (strings.HasPrefix(ln, "listens/") || strings.Contains(ln, "/listens/")):
			return importers.SourceListenBrainz, nil, nil
		case spotifyEntry(ln):
			return importers.SourceSpotify, nil, nil
		case appleEntry(ln):
			format = importers.SourceAppleMusic
			role := importers.RolePlayActivity
			if strings.Contains(ln, "daily tracks") {
				role = importers.RoleDailyTracks
			}
			if !seen[role] {
				seen[role] = true
				roles = append(roles, role)
			}
		}
	}
	if format == "" {
		return "", nil, ErrUnknownFormat
	}
	return format, roles, nil
}

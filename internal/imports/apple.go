package imports

import (
	"archive/zip"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"younithm/internal/importers"
)

// appleFileRoles says which of the two Apple Music export files an upload is,
// from its contents rather than its name. A plain CSV is one file; a zip (Apple's
// privacy export) can hold both, and each entry is classified by its header. The
// rows it returns carry the role, the file name (the entry's name for a zip) and
// the entry path; the caller fills in where the upload was stored.
func appleFileRoles(path string) ([]fileRef, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	if !isZip(head[:n]) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		role := roleOfStream(f)
		if role == "" {
			return nil, errors.New("not an Apple Music play history file: expected \"Apple Music Play Activity.csv\" or \"Apple Music - Play History Daily Tracks.csv\" (or the privacy export zip holding them)")
		}
		return []fileRef{{role: role}}, nil
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		return nil, fmt.Errorf("unreadable zip: %w", err)
	}
	var out []fileRef
	for _, e := range zr.File {
		ln := strings.ToLower(e.Name)
		if e.FileInfo().IsDir() || strings.HasPrefix(ln, "__macosx/") || !appleEntry(ln) {
			continue
		}
		rc, err := e.Open()
		if err != nil {
			return nil, err
		}
		role := roleOfStream(rc)
		rc.Close()
		if role != "" {
			out = append(out, fileRef{role: role, filename: e.Name, entry: e.Name})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("this zip holds no Apple Music Play Activity or Daily Tracks CSV")
	}
	return out, nil
}

// roleOfStream reads a CSV's header row and says which Apple file it is.
func roleOfStream(r io.Reader) string {
	br := bufio.NewReaderSize(r, 64*1024)
	line, _ := br.ReadString('\n')
	line = strings.TrimPrefix(line, "\xef\xbb\xbf")
	return importers.RoleOf(line)
}

// openAppleRole opens every stored file of a role, in the order they were added.
func openAppleRole(refs []fileRef, role string, closers *[]func()) ([]io.Reader, error) {
	var out []io.Reader
	zips := map[string]*zip.ReadCloser{}
	*closers = append(*closers, func() {
		for _, z := range zips {
			z.Close()
		}
	})
	for _, r := range refs {
		if r.role != role {
			continue
		}
		if r.path == "" {
			return nil, fmt.Errorf("the %s file is no longer stored", role)
		}
		if r.entry == "" {
			f, err := os.Open(r.path)
			if err != nil {
				return nil, err
			}
			*closers = append(*closers, func() { f.Close() })
			out = append(out, f)
			continue
		}
		z, ok := zips[r.path]
		if !ok {
			var err error
			if z, err = zip.OpenReader(r.path); err != nil {
				return nil, err
			}
			zips[r.path] = z
		}
		var found *zip.File
		for _, e := range z.File {
			if e.Name == r.entry {
				found = e
			}
		}
		if found == nil {
			return nil, fmt.Errorf("%q is not in the uploaded zip", r.entry)
		}
		rc, err := found.Open()
		if err != nil {
			return nil, err
		}
		*closers = append(*closers, func() { rc.Close() })
		out = append(out, rc)
	}
	return out, nil
}

// ParseApple joins an Apple Music job's Play Activity and Daily Tracks files into
// listens. It refuses to start unless both are stored.
func ParseApple(ctx context.Context, j Job, emit func(importers.Listen) error, onRowErr func(importers.RowError)) (importers.Result, importers.AppleJoinStats, error) {
	if err := j.requireFiles(); err != nil {
		return importers.Result{}, importers.AppleJoinStats{}, err
	}
	var closers []func()
	defer func() {
		for _, c := range closers {
			c()
		}
	}()
	play, err := openAppleRole(j.fileRefs, importers.RolePlayActivity, &closers)
	if err != nil {
		return importers.Result{}, importers.AppleJoinStats{}, err
	}
	daily, err := openAppleRole(j.fileRefs, importers.RoleDailyTracks, &closers)
	if err != nil {
		return importers.Result{}, importers.AppleJoinStats{}, err
	}
	return importers.AppleJoin(ctx, play, daily, j.Options.SpotifyMinMs, emit, onRowErr)
}

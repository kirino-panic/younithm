package secrets

import (
	"io"
	"strings"
	"sync"
)

const redacted = "[redacted]"

// minRedactLen avoids scrubbing trivially short values out of every log line.
const minRedactLen = 6

// Redactor removes known secret values from text. Register values as they
// are set or loaded; Writer wraps a log destination.
type Redactor struct {
	mu   sync.RWMutex
	vals []string
}

// NewRedactor returns a Redactor pre-loaded with values.
func NewRedactor(values ...string) *Redactor {
	r := &Redactor{}
	r.Add(values...)
	return r
}

// Add registers secret values (and their whitespace-trimmed form).
func (r *Redactor) Add(values ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < minRedactLen {
			continue
		}
		dup := false
		for _, e := range r.vals {
			if e == v {
				dup = true
				break
			}
		}
		if !dup {
			r.vals = append(r.vals, v)
		}
	}
}

// Redact replaces every registered value in s.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.vals {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	return s
}

type redactWriter struct {
	r *Redactor
	w io.Writer
}

// Write redacts each write as a unit; the standard logger writes one line per
// call, so a value is never split across calls.
func (rw redactWriter) Write(p []byte) (int, error) {
	if _, err := rw.w.Write([]byte(rw.r.Redact(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Writer returns an io.Writer that redacts before writing to w, suitable for
// log.SetOutput.
func (r *Redactor) Writer(w io.Writer) io.Writer { return redactWriter{r: r, w: w} }

// Hint returns a display hint for a secret: its last four characters, or ""
// when the value is too short for that to be safe (under 12 characters). PEM
// armor is ignored so a .p8 key does not show "KEY-".
func Hint(value string) string {
	v := strings.TrimSpace(value)
	if strings.Contains(v, "-----") {
		var body []string
		for _, ln := range strings.Split(v, "\n") {
			ln = strings.TrimSpace(ln)
			if ln != "" && !strings.HasPrefix(ln, "-----") {
				body = append(body, ln)
			}
		}
		v = strings.Join(body, "")
	}
	if len(v) < 12 {
		return ""
	}
	return v[len(v)-4:]
}

package debugui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"younithm/internal/identity"
)

// matching runs the whole matching pipeline on demand:
//
//  1. identity re-resolve: the backfill re-reads every scrobble's raw payload
//     through the identity resolver (artists, recordings, releases, ids, join
//     phrases);
//  2. provider matching: every recording is queued again for enrichment, and
//     the background worker asks MusicBrainz / Spotify / Apple for ids, ISRCs,
//     artists, albums and name variants, most-played first.
//
// Step 2 is a queue worked at the providers' rate limits, so "done" here means
// "queued"; the status endpoint shows the queue draining.
type matching struct {
	d  Deps
	mu sync.Mutex
	st runState
}

type runState struct {
	Running    bool      `json:"running"`
	Steps      []stepLog `json:"steps"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type stepLog struct {
	Name   string    `json:"name"`
	State  string    `json:"state"` // running, done, failed, skipped
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

func newMatching(d Deps) *matching { return &matching{d: d} }

func (m *matching) step(name, state, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.st.Steps {
		if m.st.Steps[i].Name == name {
			m.st.Steps[i] = stepLog{name, state, detail, time.Now()}
			return
		}
	}
	m.st.Steps = append(m.st.Steps, stepLog{name, state, detail, time.Now()})
}

func (m *matching) finish(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Running, m.st.FinishedAt = false, time.Now()
	if err != nil {
		m.st.Error = err.Error()
	}
}

// run starts the pipeline. Body: {"identity": bool, "enrichment": bool}; both
// default to true when the body is empty.
func (m *matching) run(w http.ResponseWriter, r *http.Request) {
	in := struct {
		Identity   *bool `json:"identity"`
		Enrichment *bool `json:"enrichment"`
	}{}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "bad JSON body")
			return
		}
	}
	doID, doEnr := in.Identity == nil || *in.Identity, in.Enrichment == nil || *in.Enrichment
	if !doID && !doEnr {
		fail(w, http.StatusBadRequest, "nothing selected")
		return
	}
	m.mu.Lock()
	if m.st.Running {
		m.mu.Unlock()
		fail(w, http.StatusConflict, "a matching run is already in progress")
		return
	}
	m.st = runState{Running: true, StartedAt: time.Now()}
	m.mu.Unlock()
	go m.pipeline(m.d.Background, doID, doEnr)
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

func (m *matching) pipeline(ctx context.Context, doID, doEnr bool) {
	if doID {
		if err := m.identityStep(ctx); err != nil {
			m.step("identity re-resolve", "failed", err.Error())
			m.finish(err)
			return
		}
	} else {
		m.step("identity re-resolve", "skipped", "")
	}
	if doEnr {
		m.step("provider matching", "running", "queueing every recording")
		n, err := m.d.Enrich.RequeueAll(ctx)
		if err != nil {
			m.step("provider matching", "failed", err.Error())
			m.finish(err)
			return
		}
		m.d.Enrich.Wake()
		m.step("provider matching", "done", itoa(n)+" recordings queued; the worker is draining the queue (see Queue below)")
	} else {
		m.step("provider matching", "skipped", "")
	}
	m.finish(nil)
}

func (m *matching) identityStep(ctx context.Context) error {
	const name = "identity re-resolve"
	m.step(name, "running", "starting backfill")
	id, err := m.d.Backfill.Start(ctx, false, false)
	if errors.Is(err, identity.ErrBackfillRunning) {
		m.step(name, "running", "a backfill was already running; waiting for it")
		id = 0
	} else if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		s, err := m.d.Backfill.Latest(ctx, false)
		if err != nil {
			return err
		}
		if s == nil || (id != 0 && s.ID != id) {
			continue
		}
		detail := itoa(int64(s.Report.Processed)) + " of " + itoa(s.Total) + " scrobbles"
		switch s.Status {
		case "running":
			m.step(name, "running", detail)
		case "done":
			m.step(name, "done", detail)
			return nil
		default:
			return errors.New("backfill " + s.Status + ": " + s.Error)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// status is everything the matching panel shows.
func (m *matching) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m.mu.Lock()
	st := m.st
	st.Steps = append([]stepLog(nil), m.st.Steps...)
	m.mu.Unlock()

	prev, err1 := m.d.Backfill.Latest(ctx, true)
	run, err2 := m.d.Backfill.Latest(ctx, false)
	queue, err3 := m.d.Enrich.Counts(ctx)
	integrity, err4 := identity.CheckIntegrity(ctx, m.d.Read)
	review, err5 := count(ctx, m.d.Read, `SELECT COUNT(DISTINCT entity_id) FROM external_link WHERE entity_type='recording' AND status='candidate'`)
	if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
		serverErr(w, "matching status", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run": st, "backfill_dry_run": prev, "backfill_last": run, "queue": queue,
		"integrity": integrity, "recordings_awaiting_review": review,
	})
}

// Package server wires up the HTTP router.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"younithm/internal/names"

	"younithm/internal/config"
	"younithm/internal/db"
	"younithm/internal/debugadmin"
	"younithm/internal/debugui"
	"younithm/internal/edit"
	"younithm/internal/enrich"
	"younithm/internal/identity"
	"younithm/internal/imports"
	"younithm/internal/providers"
	"younithm/internal/providers/apple"
	"younithm/internal/providers/itunes"
	"younithm/internal/providers/match"
	"younithm/internal/providers/musicbrainz"
	"younithm/internal/providers/snapshot"
	"younithm/internal/providers/spotify"
	"younithm/internal/ratelimit"
	"younithm/internal/secrets"
	"younithm/internal/sitesettings"
	"younithm/internal/stats"
	"younithm/internal/version"
	"younithm/web"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"younithm/internal/artwork"
	"younithm/internal/auth"
	"younithm/internal/ingest"
)

// Option customizes New.
type Option func(*options)

type options struct {
	box *secrets.Box
	red *secrets.Redactor
	bg  context.Context
}

// WithBackground sets the context that bounds background workers (the import
// job runner). Defaults to context.Background().
func WithBackground(ctx context.Context) Option { return func(o *options) { o.bg = ctx } }

// WithSecrets supplies the master-key box and log redactor (main builds them
// at startup so a bad key fails fast). Without it New resolves the key itself
// from the environment and the database's directory.
func WithSecrets(box *secrets.Box, red *secrets.Redactor) Option {
	return func(o *options) { o.box, o.red = box, red }
}

func New(pools *db.Pools, opts ...Option) http.Handler {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	cfg := config.Load()
	if o.bg == nil {
		o.bg = context.Background()
	}
	if o.box == nil {
		box, _, err := secrets.OpenBox(secrets.KeyOptions{ConfigDir: filepath.Dir(pools.Path)})
		if err != nil {
			panic("server: master key: " + err.Error())
		}
		o.box = box
	}
	if o.red == nil {
		o.red = secrets.NewRedactor()
	}
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)

	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		if err := pools.Write.PingContext(ctx); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": version.Version, "time": time.Now().UTC()})
	})

	authOpt, err := auth.OptionsFromConfig(cfg)
	if err != nil {
		panic("server: " + err.Error()) // main validates config first
	}
	auth.Mount(r, pools.Write, authOpt)

	store := secrets.NewStore(pools.Read, pools.Write, o.box, o.red)
	if _, failed, err := store.LoadIntoRedactor(context.Background()); err != nil || failed > 0 {
		log.Printf("secrets: loading stored secrets into log redactor: failed=%d err=%v", failed, err)
	}
	// Connecting (or changing) a provider means ids and names from it can now be
	// filled in for the whole library: queue every recording again, once the
	// admin has stopped typing. The manager exists further down.
	var requeue func()
	var requeueMu sync.Mutex
	var requeueTimer *time.Timer
	requeueSoon := func() {
		requeueMu.Lock()
		defer requeueMu.Unlock()
		if requeueTimer != nil {
			requeueTimer.Stop()
		}
		requeueTimer = time.AfterFunc(20*time.Second, func() {
			if requeue != nil {
				requeue()
			}
		})
	}
	mountSecretsAdmin(r, store, requeueSoon)

	// Site settings: per-section public visibility.
	site := sitesettings.NewStore(pools.Read, pools.Write)
	site.SetDebug(config.DebugTools()) // YOUNITHM_DEBUG: the Debug tab and its endpoints
	sitesettings.Mount(r, site)
	sitesettings.MountPage(r, site)       // home page document (theme, tiles, custom CSS)
	sitesettings.MountProfile(r, site)    // display name and avatar
	sitesettings.MountOnboarding(r, site) // whether the welcome flow was finished
	sitesettings.MountScrobbling(r, site) // whether scrobblers need a token

	// Public read endpoints: per-IP rate limit (admin sessions exempt), then
	// the section visibility gate.
	proxies, _ := cfg.ProxyPrefixes() // validated at startup
	limiter := ratelimit.New(ratelimit.Options{Proxies: proxies, TrustAll: cfg.TrustAllProxies})
	public := func(section string) func(http.Handler) http.Handler {
		gate := site.Gate(section)
		return func(next http.Handler) http.Handler { return limiter.Middleware(gate(next)) }
	}

	identify := func(ctx context.Context, tx *sql.Tx, l ingest.NewScrobble) (int64, int64, error) {
		in := identity.FromBasics(l.RawPayload, identity.Basics{
			Artist: l.Artist, Title: l.Title, Album: l.Album, DurationMs: l.TrackDurationMs,
			RecordingMBID: l.RecordingMBID, ReleaseMBID: l.ReleaseMBID, ISRC: l.ISRC, ArtistMBIDs: l.ArtistMBIDs,
		})
		return (&identity.Resolver{}).Resolve(ctx, tx, in)
	}
	ing := ingest.New(pools.Read, pools.Write, ingest.Options{
		Auth: sitesettings.ScrobbleAuth{Site: site}, // open, or the chosen token

		RequireAdmin:    auth.RequireSession, // scrobble detail exposes raw payloads
		DedupeTolerance: config.DedupeWindow(),
		Identify:        identify, // the identity graph finds or creates the entities (aliases included)
		ListGate:        public("scrobbles"),
		NowPlayingGate:  public("now_playing"),
	})
	ing.MountRoot(r)
	ing.SetFakeNowPlaying(
		func() bool { return site.FakeNowPlaying(context.Background()) },
		func() (ingest.NowPlayingTrack, bool) {
			t, ok := debugadmin.RandomTrack(context.Background(), pools.Read)
			return ingest.NowPlayingTrack{Artist: t.Artist, Title: t.Title, Album: t.Album, Image: t.Image, DurationMs: t.DurationMs}, ok
		})
	cfgDir := filepath.Dir(pools.Path)
	sitesettings.MountDebug(r, site, sitesettings.DebugHooks{
		Counts: func(ctx context.Context) (debugadmin.Counts, error) { return debugadmin.Count(ctx, pools.Read) },
		Clear: func(ctx context.Context) error {
			return debugadmin.Clear(ctx, pools.Write, filepath.Join(cfgDir, "imports"), filepath.Join(cfgDir, "artwork"))
		},
		OnFakeNowPlaying: ing.PokeNowPlaying,
	})
	nm := names.New(pools.Read, pools.Write)                      // display-name preference
	var onLinkChanged atomic.Pointer[func(kind string, id int64)] // set once enrichment exists
	var onRefresh atomic.Pointer[func(kind string, id int64) error]
	edit.Mount(r, pools, edit.Hooks{LinkChanged: func(kind string, id int64) {
		if f := onLinkChanged.Load(); f != nil {
			(*f)(kind, id)
		}
	}, Refresh: func(kind string, id int64) error {
		if f := onRefresh.Load(); f != nil {
			return (*f)(kind, id)
		}
		return errors.New("lookups are not running yet")
	}, NamesChanged: nm.Forget, ArtDir: filepath.Join(filepath.Dir(pools.Path), "artwork")}) // admin editing endpoints + edit history

	var afterImport atomic.Pointer[func()] // set once the enrichment manager exists
	imp := imports.NewManager(pools.Read, pools.Write, imports.Options{
		Dir:          filepath.Join(filepath.Dir(pools.Path), "imports"),
		LiveWindow:   config.DedupeWindow(),
		ImportWindow: config.ImportDedupeWindow(),
		MaxBytes:     config.ImportMaxBytes(),
		DebugLimit:   func() int { return site.ImportLimit(context.Background()) },
		OnDone: func() {
			if f := afterImport.Load(); f != nil {
				(*f)()
			}
		},
	})
	if err := imp.Start(o.bg); err != nil {
		log.Printf("imports: starting job runner: %v", err)
	}

	backfill := identity.NewBackfill(o.bg, pools.Write, pools.Path)
	if err := backfill.Recover(o.bg); err != nil {
		log.Printf("identity: recovering backfill jobs: %v", err)
	}

	// Enrichment: provider matching worker (needs a MusicBrainz contact) and
	// the admin review queue for ambiguous matches.
	reg := providers.NewRegistry()
	// MusicBrainz needs a contact for its User-Agent: the one saved in the
	// encrypted store (set at runtime), else MUSICBRAINZ_CONTACT. Unset = skipped.
	mb := musicbrainz.NewDynamic(func(ctx context.Context) (string, error) {
		v, err := store.Get(ctx, "musicbrainz.contact")
		if errors.Is(err, secrets.ErrNotSet) {
			if cfg.MusicBrainzContact == "" {
				return "", providers.ErrNotConfigured
			}
			return cfg.MusicBrainzContact, nil
		}
		return v, err
	}, musicbrainz.Config{})
	reg.Add(mb)
	// Spotify credentials are entered in the admin UI at runtime, so they are
	// read from the encrypted store on each token fetch. Unset = skipped.
	if sp, err := spotify.New(spotify.Config{Credentials: func(ctx context.Context) (string, string, error) {
		id, err := store.Get(ctx, "spotify.client_id")
		if err != nil {
			if errors.Is(err, secrets.ErrNotSet) {
				return "", "", providers.ErrNotConfigured
			}
			return "", "", err
		}
		secret, err := store.Get(ctx, "spotify.client_secret")
		if err != nil {
			if errors.Is(err, secrets.ErrNotSet) {
				return "", "", providers.ErrNotConfigured
			}
			return "", "", err
		}
		return id, secret, nil
	}}); err == nil {
		reg.Add(sp)
	}
	// Apple Music (developer-token JWT from the .p8 key) when its three
	// secrets are set. The keyless iTunes Search API can stand in until then, but
	// only when explicitly enabled (YOUNITHM_ENABLE_ITUNES): it is never on by default.
	appleSecret := func(ctx context.Context, name string) (string, error) {
		v, err := store.Get(ctx, name)
		if errors.Is(err, secrets.ErrNotSet) {
			return "", providers.ErrNotConfigured
		}
		return v, err
	}
	if ap, err := apple.New(apple.Config{Credentials: func(ctx context.Context) (string, string, string, error) {
		team, err := appleSecret(ctx, "apple.team_id")
		if err != nil {
			return "", "", "", err
		}
		kid, err := appleSecret(ctx, "apple.key_id")
		if err != nil {
			return "", "", "", err
		}
		pk, err := appleSecret(ctx, "apple.private_key")
		return team, kid, pk, err
	}}); err == nil {
		reg.Add(ap)
	}
	mountConnectionCheck(r, reg)
	if cfg.EnableITunes {
		reg.Add(providers.When(itunes.New(itunes.Config{}), func(ctx context.Context) bool {
			ok, err := store.Has(ctx, "apple.private_key")
			return err == nil && !ok
		}))
	}
	enr := enrich.NewManager(pools.Write, match.New(reg, match.Config{}), snapshot.New(pools.Write), enrich.Options{
		Merge: func(ctx context.Context, kind string, from, into int64) error {
			_, err := (&edit.Editor{DB: pools.Write}).Merge(ctx, "matching", kind, from, into)
			return err
		},
	})
	requeue = func() {
		if n, err := enr.RequeueAll(o.bg); err != nil {
			log.Printf("enrich: requeue after provider change: %v", err)
		} else {
			log.Printf("enrich: provider settings changed; queued %d recordings", n)
		}
	}
	sweep := enr.SweepNow
	afterImport.Store(&sweep)
	relink := func(kind string, id int64) {
		nm.Forget()
		var err error
		switch kind {
		case "recording": // look the linked track up so its details fill in
			err = enr.Requeue(o.bg, id)
		case "release": // the album's tracklist may now place its tracks
			err = enr.RequeueRelease(o.bg, id)
		default:
			return
		}
		if err != nil {
			log.Printf("enrich: requeue after link edit: %v", err)
		}
		enr.Wake()
	}
	onLinkChanged.Store(&relink)
	refresh := func(kind string, id int64) error {
		var err error
		if kind == "release" {
			err = enr.RequeueRelease(o.bg, id)
		} else {
			err = enr.Requeue(o.bg, id)
		}
		if err == nil {
			enr.Wake()
		}
		return err
	}
	onRefresh.Store(&refresh)
	go enr.Run(o.bg) // providers without credentials are skipped per call
	// Covers and artist photos: downloaded from the linked providers into
	// <config>/artwork and served at /api/v1/art/{kind}/{id}. Not rate limited
	// or gated: a page loads many at once.
	artDir := filepath.Join(filepath.Dir(pools.Path), "artwork")
	art := artwork.New(pools.Read, pools.Write, artDir, reg, artwork.Options{})
	go art.Run(o.bg)
	r.Get("/api/v1/art/{kind}/{id}", artwork.Handler{Read: pools.Read, Dir: artDir}.ServeHTTP)
	if cfg.MusicBrainzContact == "" {
		log.Printf("enrich: MUSICBRAINZ_CONTACT not set; MusicBrainz matching stays off until a contact is saved (Spotify and Apple run once their keys are entered)")
	}
	review := enrich.NewReview(pools.Read, enr, snapshot.New(pools.Read))
	review.Mount(r, auth.RequireSession)

	if cfg.DebugUI { // developer UI: see internal/debugui
		debugui.Mount(r, debugui.Deps{
			Read: pools.Read, Write: pools.Write, Secrets: store, Registry: reg,
			MusicBrainz: mb, MusicBrainzEnvContact: cfg.MusicBrainzContact, ITunesEnabled: cfg.EnableITunes,
			Imports: imp, Backfill: backfill, Enrich: enr, Review: review, Background: o.bg,
		})
	}

	nm.Mount(r, auth.RequireSession)
	nm.MountLanguages(r, auth.RequireSession, enr.NamesNow) // names in the preferred languages
	r.Route("/api/v1", func(r chi.Router) {
		ing.MountAPI(r)
		imp.Mount(r)                           // admin import jobs
		backfill.Mount(r, auth.RequireSession) // identity backfill
		st := stats.New(pools.Read).WithNames(nm)
		st.Mount(r, stats.Gates{
			Recent: public("recent"), Top: public("top"), Stats: public("stats"),
		})
		st.MountLibrary(r, public("library"), site.Gate("recent"))
	})

	r.NotFound(web.Handler(config.BasePath()).ServeHTTP) // the embedded frontend

	return withBasePath(config.BasePath(), r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

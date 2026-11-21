# Architecture

younithm is a single process: an HTTP server, a few background workers and a SQLite database. The React frontend is built into `web/dist` and embedded in the binary.

## Code layout

```
cmd/younithm/          entrypoint; `serve` (default) and `reset-admin`
internal/server/       router wiring, health check, secrets and connection endpoints
internal/config/       environment variables
internal/db/           SQLite pools, embedded goose migrations, backups
internal/auth/         admin account, sessions, CSRF, throttling, step-up
internal/secrets/      master key, encrypted provider secrets, log redaction
internal/sitesettings/ site settings: visibility, home page, profile, onboarding, scrobble token, debug tools
internal/ingest/       ListenBrainz-compatible submit API, now playing, scrobble list
internal/identity/     resolver: listen → artists, recording, release; backfill; integrity check
internal/importers/    pure streaming parsers for export files
internal/imports/      import jobs: upload, preview, confirm, apply
internal/providers/    MusicBrainz, Spotify, Apple Music, iTunes clients; matching ladder; snapshot cache
internal/enrich/       enrichment worker, review queue, names in preferred languages
internal/names/        display-name and language preferences
internal/edit/         editing, merge, aliases, bulk edit, edit log and undo
internal/artwork/      artwork worker and image serving
internal/stats/        public read API: recent, top, stats, entity pages, library
internal/ratelimit/    per-IP token bucket for public endpoints
internal/debugadmin/   counts and library wipe used by the debug tools
internal/debugui/      developer UI (off by default)
web/                   React + Vite frontend
docker/                container entrypoint
```

## Data model

Scrobbles are events that point at canonical entities. What a scrobbler or file submitted is kept on the scrobble and never overwritten; the entities can be edited, merged and enriched freely.

| Table | Holds |
|---|---|
| `scrobble` | `played_at`, `source`, `raw_artist`, `raw_title`, `raw_album`, `raw_payload`, and the `recording_id` / `release_id` it points at |
| `artist`, `recording`, `release` | Our own rows. These are the identity; provider ids attach to them |
| `recording_artist`, `release_artist` | Credits, in order, with join phrases |
| `recording_release` | Every album a recording is on; one is primary |
| `external_link` | Provider ids (MusicBrainz, Spotify, Apple, ISRC, barcode) per entity, with method, confidence and status (`confirmed`, `auto`, `candidate`) |
| `alias` | Name variants that resolve to an entity |
| `entity_name` | Names each catalogue returns, per language |
| `merge_candidate` | Pairs that might be duplicates, for a person to decide |
| `provider_snapshot` | Cached provider responses |
| `enrichment_job` | The enrichment queue |
| `import_job` | Import jobs, their preview and result |
| `edit_log` | Every edit, with enough detail to undo it |

A track is a **recording**. Plays count per recording, across every album it is on. Each scrobble also records the album it was played from, when known.

## Request flow

- **Scrobbles** arrive at `/1/submit-listens`. `ingest.Store.Insert` dedupes them, runs each through the identity resolver, and writes it. No network calls happen on this path.
- **Imports** are parsed from disk, previewed, and applied in batches through the same resolver and dedupe rules.
- **Enrichment** runs later in the background: it asks providers about the ids each recording holds, adds what it learns, and finds the recording on the providers it has no id for.

## Background workers

All run in-process, bound to the server's lifetime.

| Worker | Does |
|---|---|
| Import worker | One import job at a time |
| Enrichment | Polls the queue every 15 s, most-played first; sweeps for unqueued recordings every 10 minutes |
| Names pass | Fills artist links and names in preferred languages when the queue is idle |
| Artwork | Every 30 s, fetches images for up to 25 linked releases and artists |

## Database

SQLite in WAL mode via `modernc.org/sqlite` (pure Go, no CGO). `db.OpenPools` returns a single-connection `Write` pool for all writes and a `Read` pool for queries. Migrations are embedded and run at startup; when any are pending, a copy of the database is first written to `<config>/backups/` (the newest five are kept).

## Frontend

React with React Router, built by Vite. Public routes: the home page (a tile dashboard the admin designs), artist, album and track pages, scrobbles and library. Admin routes: settings, imports, review, connections. `/setup` and `/welcome` handle first run.

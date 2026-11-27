# Imports

Upload an export from another service, check the preview, confirm. Code: `internal/importers` (pure streaming parsers) and `internal/imports` (jobs, dedupe, HTTP).

## Formats

| Format | Accepts |
|---|---|
| `listenbrainz` | JSONL, JSON array, API envelope, or the export zip (`listens/<year>/<month>.jsonl`) |
| `lastfm` | CSV (with header, or headerless 4 columns) and JSON recent-tracks pages |
| `spotify` | Extended streaming history (`Streaming_History_Audio_*.json`, `endsong_*.json`) and the older `StreamingHistory*.json`, as files or a zip. Plays under 30 s are skipped (configurable per upload) |
| `maloja` | Maloja JSON export |
| `koito` | Koito JSON export |
| `applemusic` | Apple's privacy export: two CSVs, see below |

The format is detected from the first 64 KiB or the zip's entry names. When detection fails, the upload returns `422 unknown_format` and the format must be named.

Uploads are streamed to disk, hashed, and parsed from disk; nothing is loaded whole.

## Lifecycle

```
queued → running (preview) → awaiting_confirmation → confirm → queued → running (apply) → done
any unfinished state → cancel → cancelled
running → error or restart → failed → retry → queued
```

- **Preview has no side effects.** It reports counts, date range, duplicates, how listens will match, and samples.
- **Confirm applies exactly what was previewed.** Options are frozen on the job, and the file is re-hashed before applying. The database may change in between (a live scrobble arrives), so the result includes `drift`: preview versus applied counts.
- **Apply is resumable.** Rows are written in batches of 500 and carry `scrobble.import_job_id`. After a restart, a running job is marked `failed`; retry picks up where it stopped.
- **Confirm early.** `confirm?when_ready=1` accepts a job that is still waiting for files or being previewed; it applies as soon as the preview finishes. The welcome flow uses this.
- **Date range.** `from` and `to` (YYYY-MM-DD, both inclusive, either optional) keep only listens played on or between those days, in the uploader's time zone (`tz`, UTC when absent). Listens outside are left out before dedupe and counted in the preview's `out_of_range`; the file's own span stays in `date_range`.
- Uploads are deleted when a job is done or cancelled. One worker runs one job at a time.

## Apple Music

Apple splits what an import needs across two files, and an Apple import needs both:

- **`Apple Music Play Activity.csv`**: every play with its time, song name, album name and durations. No artist and no catalogue id.
- **`Apple Music - Play History Daily Tracks.csv`**: per track and day, the Apple song id and `Artist - Title`.

The job waits in `awaiting_files` until both are uploaded, either separately (`POST /admin/imports` then `POST /admin/imports/{id}/files`) or as the export zip. Nothing is read, previewed or written until then. Files are told apart by their header.

The join matches each play to the Daily Tracks row with the same title on the same day, or the day either side when that day has none. A play with no match, or more than one song id for that title and day, is skipped and counted, never guessed. Radio stations and plays under 30 s are skipped too, and so is every Play Activity event other than `PLAY_END`: `PLAY_START` is only a start marker and `LYRIC_DISPLAY` is lyrics on screen, so counting them counts one listen two or three times. Every imported row then resolves by its Apple id, and enrichment replaces the artist string with Apple's credits and fills the album.

The preview's `apple` block reports `daily_rows`, `daily_tracks`, `joined`, `unjoined`, `ambiguous` and the first 20 skipped plays with reasons.

## Mapping

Raw artist, title and album are stored exactly as in the file. `played_at` is UTC to the second. `source` is `import:<format>`. `raw_payload` keeps the source record with personal data removed at any depth: IP addresses, usernames, email, user agent and country. A payload that can't be scrubbed is dropped.

## Matching

An import never calls the network. Each listen goes through the same resolver as live scrobbles ([identity.md](identity.md)). The preview counts listens matched **by id** (a MusicBrainz, Spotify, Apple or ISRC id was present) and **by name**.

`fuzzy_consent` decides whether enrichment may later link by-name listens automatically on a fuzzy match. Without it, such matches wait in the review queue.

## Dedupe

A listen is a duplicate if:

1. an earlier row in the same file has the same second, normalised artist and title, or
2. a stored scrobble has the same normalised artist and title within the dedupe window.

The import window defaults to 30 s (`YOUNITHM_IMPORT_DEDUPE_WINDOW`, up to 1 h), can be set per upload, and is never below the live window. Exporters disagree about timestamps by seconds, so a Spotify history that overlaps live scrobbles shouldn't count twice. Re-importing the same file is always a no-op.

The preview explains each duplicate: which scrobble it matched and the offset.

## API

Under `/api/v1/admin`, admin session required. Writes need the CSRF header; confirm and retry also need step-up.

| Endpoint | Does |
|---|---|
| `POST /imports` | Multipart `file`. Options: `format`, `dedupe_window`, `spotify_min_ms`, `fuzzy_consent`, `debug_limit`, `from`, `to`, `tz` |
| `POST /imports/check` | `{"head": base64}` or `{"entries": [...]}`: detect format (and Apple file role) without storing anything |
| `GET /imports` | Jobs, supported formats, required files, defaults |
| `GET /imports/{id}` | One job with progress, preview, result and error |
| `POST /imports/{id}/files` | Add a file to a job awaiting files; `?replace=true` swaps one |
| `POST /imports/{id}/confirm` | Apply the preview (`?when_ready=1` to confirm early) |
| `POST /imports/{id}/retry` | Re-run a failed job |
| `POST /imports/{id}/cancel` | Cancel an unfinished job |
| `DELETE /imports/{id}` | Remove a finished job's record; its scrobbles stay |
| `POST /imports/{id}/undo` | Remove every scrobble a finished import added, then the job, so the file can be imported again. Backs up the database first; needs step-up |

Errors are `{"error": code, "message": text}`, for example `unknown_format` (422), `too_large` (413), `invalid_state` (409), `step_up_required` (403).

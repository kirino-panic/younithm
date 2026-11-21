# Editing

Fix a single scrobble, an entity everywhere, or merge duplicates, with every change undoable. Code: `internal/edit`. All endpoints are admin-only; merge and bulk apply also need step-up.

## Model

- A scrobble's `raw_*` fields and `raw_payload` are never edited. What it points at (`recording_id`, `release_id`) and `played_at` are. The first edit of `played_at` saves the old value in `orig_played_at`.
- Display values come from the entities, so renaming a recording, artist or album changes every scrobble at once.
- Renaming keeps the old name as an alias, so future listens with that spelling still resolve.

## Edit log and undo

Every edit writes its row-level changes and one `edit_log` entry in the same transaction. `POST /edits/{id}/undo` replays the changes in reverse. It first checks that every changed column still holds what the edit left there; if not, it refuses with `409 undo_conflict` and lists the conflicts, writing nothing. An undo is itself logged, so undoing it redoes the original. Undoing a merge or bulk edit needs step-up.

## Merge

Merging A into B (same kind) moves A's scrobbles, credits, album memberships, links and aliases to B, deletes A, and adds A's names as aliases of B. One undoable entry.

## Aliases

Aliases map a normalised name (lowercase, trimmed, collapsed whitespace) to an entity:

| Kind | Key |
|---|---|
| artist | name |
| recording | artist + title |
| release | title |

The resolver checks aliases before matching by name, at ingest and import. Aliases come from edits, merges and enrichment.

## Albums

A scrobble's album must be one its recording is on, unless the edit links it in the same step (`link_release`). Re-pointing a scrobble at a recording that isn't on its album drops the album. A recording's first album becomes primary. Removing a recording from its primary album promotes the earliest remaining one.

## Bulk edit

Filter by any of `ids`, `from`, `to`, `source`, `raw_artist`, `raw_title`, `raw_album`, `recording_id`, `release_id`, `artist_id`, `unmatched`. Set `recording_id`, `release_id`, `link_release` or `shift_seconds`. Preview first; apply must send the preview's `expected_matched`, or it returns `409 count_changed`. Up to 50,000 rows, one log entry, one undo.

## Endpoints

Under `/api/v1/admin`.

| Endpoint | Does |
|---|---|
| `GET /edits` | History, newest first (`entity_type`, `entity_id`, `scope`, `before_id`, `limit`) |
| `GET /edits/{id}` | One entry with before and after |
| `POST /edits/{id}/undo` | Undo |
| `GET /scrobbles/{id}` | Raw and effective values |
| `PATCH /scrobbles/{id}` | `played_at`, `recording_id`, `release_id`, `link_release`, `new_recording`, `new_release`; absent means unchanged, `null` clears |
| `DELETE /scrobbles/{id}` | Delete; undo restores it |
| `POST /scrobbles/{id}/move-album` | `{release_id, link_release}` |
| `POST /scrobbles/bulk/preview` | `{filter, set}`: what would change; writes nothing |
| `POST /scrobbles/bulk/apply` | `{filter, set, expected_matched}` (step-up) |
| `PATCH /entities/{kind}/{id}` | Edit fields (`kind`: artist, recording, release); `drop_alias` skips keeping the old name |
| `PUT /entities/{kind}/{id}/artists` | Replace credits |
| `PUT /entities/{kind}/{id}/links` | Link a provider URL |
| `DELETE /entities/{kind}/{id}/links/{provider}` | Remove a link |
| `POST`/`DELETE /entities/{kind}/{id}/image` | Upload or reset an image override |
| `POST /entities/{kind}/{id}/merge` | `{into}` (step-up) |
| `GET /entities/{kind}/{id}/merge-candidates` | Likely duplicates of this entity |
| `GET /entities/{kind}/candidates` | Search for merge targets |
| `POST /recordings/{id}/releases` | Add to an album |
| `DELETE /recordings/{id}/releases/{release_id}` | Remove from an album |
| `PUT /recordings/{id}/primary-release` | `{release_id}` |
| `GET /search` | Find artists, recordings or releases by name (`kind`, `q`) |

`new_recording` (`{title, artists}`) points the scrobble at the existing recording with that title and exactly those artists, or creates one. `new_release` (a title) creates an album with the track's artists. Both are for single scrobbles only.

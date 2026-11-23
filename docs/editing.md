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

A merge with `once` (the merge dialog's "Apply changes to all further scrobbles" switched off) moves what A has now but drops A's names, aliases and catalogue links instead of giving them to B, so a later listen named or linked like A makes a new entry.

## Split

Splitting artist A ("Danny Brown & Underscores") into artists B and C replaces A's credit on every track and album with B and C, in A's place in the credit list (an artist already credited there keeps its row), and deletes A. The join phrases between B and C come from the split; the last takes A's. A's name, display name and aliases become rules in `artist_split`, so later listens credited that way resolve to B and C. Merging B away repoints its rules; a one-time merge drops them. Logged with the merge scope, so it and its undo need step-up.

## Delete

Deleting an entity deletes every scrobble of it. A track takes its scrobbles. An album takes the scrobbles played from it and the tracks that were only on it and have no scrobbles left. An artist takes the scrobbles of every track it is credited on, the tracks and albums credited to it alone, and its credit on shared ones; a scrobble left on a deleted album loses its album. Aliases, links and split rules go too, so a later listen with the same name makes a new entry. At most 50,000 scrobbles; logged as bulk, so it and its undo need step-up.

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

An album's release date takes a year, a year and month, or a full date (`2024`, `2024-05`, `2024-05-17`). Cleared, it is filled again from the catalogues.

"Apply to all tracks on album" credits every track on an album to the album's artists. It is one edit with one undo, and tracks already credited that way are left out. Enrichment treats each track it changed as edited by hand, so a catalogue's credits don't replace it.

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
| `POST /releases/{id}/apply-artists` | The album's artists onto every track on it |
| `POST /entities/{kind}/{id}/refresh` | Look a recording, or every recording on a release, up again; fills what's missing, like lengths. Not an edit |
| `PUT /entities/{kind}/{id}/links` | Link a provider URL |
| `DELETE /entities/{kind}/{id}/links/{provider}` | Remove a link |
| `POST`/`DELETE /entities/{kind}/{id}/image` | Upload or reset an image override |
| `POST /entities/{kind}/{id}/merge` | `{into, once}` (step-up) |
| `DELETE /entities/{kind}/{id}` | Delete with every scrobble of it (step-up) |
| `POST /artists/{id}/split` | `{artists}`: two or more, by `artist_id` or `name`, with `join_phrase` (step-up) |
| `GET /entities/{kind}/{id}/merge-candidates` | Likely duplicates of this entity |
| `GET /entities/{kind}/candidates` | Search for merge targets |
| `POST /recordings/{id}/releases` | Add to an album |
| `DELETE /recordings/{id}/releases/{release_id}` | Remove from an album |
| `PUT /recordings/{id}/primary-release` | `{release_id}` |
| `GET /search` | Find artists, recordings or releases by name (`kind`, `q`) |

`new_recording` (`{title, artists}`) points the scrobble at the existing recording with that title and exactly those artists, or creates one. `new_release` (a title) creates an album with the track's artists. Both are for single scrobbles only.

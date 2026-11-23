# Identity

How a listen becomes artists, a recording and an album. Code: `internal/identity` (the resolver, run at ingest and import) and `internal/enrich` (what is learned later from providers).

## Principles

1. **Our own row is the identity.** An `artist`, `recording` or `release` row is the entity. MusicBrainz, Spotify, Apple Music and ISRC ids are `external_link` rows attached to it, several per provider if needed.
2. **No primary provider.** MusicBrainz, Spotify and Apple Music are equals, and ISRC is the key between them. Any one, or none, may be connected. Ids from other platforms are ignored.
3. **A duplicate is better than a wrong merge.** Duplicates can be merged later, with undo. A wrong merge mixes two histories. When unsure, the resolver creates a new row and records a candidate.
4. **Raw values are never overwritten.**
5. **Every credited artist is its own entity**, with its own page, links and plays.
6. **Artist strings are taken as written.** `A feat. B` or `Coheed and Cambria` is one artist named exactly that until a catalogue says otherwise. Nothing guesses at splits.

## Keys

| Entity | Identified by |
|---|---|
| Recording | MusicBrainz recording id, Spotify track id, Apple song id; ISRC only together with a matching title |
| Artist | MusicBrainz, Spotify or Apple artist id; name as a weak fallback |
| Album | One per release (edition): MusicBrainz release id, Spotify or Apple album id, barcode; else title |

Editions are separate albums. The MusicBrainz release group is stored only to list sibling editions on an album page. Live versions, remixes, remasters and instrumentals are separate recordings.

## Resolving a listen

No network calls; this runs at ingest and import.

1. **Credits.** Take the client's artist list (`artist_names`), else MusicBrainz mapping credits, else the single artist string as one artist. A listen with no artist gets "Unknown Artist".
2. **Artist ids** pair with credits by position only when both lists have the same length.
3. **Split artists.** A credit whose normalised name an admin split into several artists (see [Editing](editing.md#split)) becomes those artists, in order, with the split's join phrases. Nothing is split by the shape of a name.
4. **Each credit** is found by any of its ids. Ids that point at two different artists never merge them: both are kept and a candidate is recorded. With no id match, the **single-match rule** applies: link by normalised name only if exactly one artist has that name and none of its ids from the same provider conflict. That artist stays *unverified* until an id confirms it. Otherwise a new unverified artist is created.
5. **Recording**: by its ids, then by alias, then by first artist and exact title; else created. Every listen with a title ends with a recording; a titled listen that maps to none is refused.
6. **Album**: by its ids, then by title; else created. Album artists come only from album-artist data, never from the track's artists, so a compilation does not split into one album per track artist. A listen with an album title but no album ids or artist attaches to the one album with that title; if several share it, a candidate is recorded instead. A listen with no album gets no release.
7. **Display** uses credit names with their join phrases.

## What enrichment adds

Enrichment (see [providers.md](providers.md)) only adds ids, names, artwork and corrections. It never decides whether an entity exists.

- **Catalogue lookups.** For each id a recording holds, the provider is asked for the ISRC, credits, album and their ids. The ISRC then finds the recording on the other providers.
- **Credits from a catalogue the listen named.** When a listen carried a catalogue id, that catalogue's credit list replaces the artist string, in its order with its join phrases. An artist that drops out becomes a recording alias, so the same listen still resolves.
- **Ids we found ourselves never replace credits.** A link found by ISRC or a fuzzy search attaches ids only to artists with an equal name or an id we already hold. It never pairs differently named artists by position.
- **A fuller list adds artists.** Another catalogue may add artists, never remove them, and only when every artist we hold pairs with one of its credits by id or equal name.
- **Barcodes tie albums together.** Album barcodes from Spotify and Apple are stored as `upc` links. An album with a barcode attaches to the release holding that barcode, whatever it is called. A release holding only other barcodes is another edition. When a barcode proves two spellings of one album (Apple's "Title - Single" and Spotify's "Title"), and one side has no ids of its own, they are merged through the editor (undoable, actor `matching`); otherwise they are flagged for review.
- **Gaps are filled.** A recording with no album gets the one a catalogue names, preferring the album the listen itself named. Plays with no album of their own get the recording's primary release. An album's date comes from the album lookup, never from a song's own date.
- **Names become aliases.** Every name a provider returns is added as an alias of what it matched, unless it is another entity's name.
- **Conflicts are flagged, not moved.** An id already owned by another entity becomes a `merge_candidate`.
- **Album artists get the catalogue's artist ids.** When a catalogue's album is attached to one of our releases, an album that has no artists gets the album's credits. An album that has artists has each one paired with the album's credits by an id it already holds or an equal name, never by position, and given that credit's id. An id another of our artists holds makes the two a merge candidate. An artist holding another id of that catalogue is left alone.
- **Duplicates with proof are merged.** Two recordings are merged through the editor (undoable, actor `matching`, the older one kept) when ids prove them one song: their catalogue answers share an ISRC and their lengths agree, or the tracklist of an album both are on (one the listen named or a person linked) names them as one track and their own lengths, where known, are within 3 seconds. Recordings holding different MusicBrainz recordings are never merged this way. A merge a person undoes is not made again; the pair stays flagged.
- **Hand edits win.** A recording with an entry in the edit log keeps its artists and albums.

## Stats

A play counts for every credited artist on its recording. Total play counts stay one per scrobble.

## Backfill and integrity

`identity.Backfill` re-runs every scrobble's `raw_payload` through the resolver as a resumable job, with a dry run on a private copy first. Existing rows are kept and new data attaches to them, so merges and aliases survive.

| Endpoint | Does |
|---|---|
| `GET /api/v1/admin/identity/backfill` | Latest dry run and real run |
| `POST /api/v1/admin/identity/backfill/preview` | Start a dry run |
| `POST /api/v1/admin/identity/backfill` | Start the real run, `{"resume": bool}` (step-up) |
| `POST /api/v1/admin/identity/backfill/cancel` | Stop the running job |
| `GET /api/v1/admin/identity/integrity` | Rule checks and counts awaiting review |

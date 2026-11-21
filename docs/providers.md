# Providers and enrichment

Metadata comes from MusicBrainz, Spotify and Apple Music, using your own credentials. Nothing is sent to a provider you haven't set up. What enrichment writes, and why, is in [identity.md](identity.md).

## Providers

| Provider | Credentials | Notes |
|---|---|---|
| MusicBrainz | A contact (email or URL) | Sent in the User-Agent as `younithm/<version> ( <contact> )`. 1 request per second, shared across all callers. Skipped until a contact is set |
| Spotify | Client ID and secret | Client-credentials flow only |
| Apple Music | Team ID, key ID, `.p8` private key | The server signs its own developer-token JWT |
| iTunes Search | None | Off unless `YOUNITHM_ENABLE_ITUNES=1`, and then used only while Apple Music keys are unset |

Credentials are entered under **Admin → Connections** and stored encrypted (see [security.md](security.md)). They are read on every call, so changes apply without a restart. Saving new provider keys queues every recording again after a 20 s pause. `MUSICBRAINZ_CONTACT` is a fallback; a contact saved in Connections wins.

MusicBrainz answers `503` when the rate is exceeded. The client honours `Retry-After` when present, otherwise waits 5 s, and handles both 503 and 429.

## Packages

| Package | Role |
|---|---|
| `internal/providers` | `Provider` interface, optional lookups (ISRC, album, barcode, release tracks, track id), ordered registry, shared limiter, errors |
| `internal/providers/musicbrainz`, `spotify`, `apple`, `itunes` | Stateless clients; they never touch the database |
| `internal/providers/match` | The matching ladder, normalisation and scoring |
| `internal/providers/snapshot` | `provider_snapshot` cache with positive and negative entries |
| `internal/enrich` | Queue, worker, review endpoints, names pass |

## Enrichment

Every recording is queued, most-played first. For each one:

1. **Held ids.** Each provider is asked about the ids the recording holds. This yields ISRCs, credits, albums, album artists, dates and release groups.
2. **Missing providers.** For providers it holds no id for, the ladder runs: ISRC lookup, then the tracklist of the release the listen named, then a search that loosens step by step.
3. **Write back.** Links, aliases and gap fills are applied by the rules in [identity.md](identity.md).

A recording with nothing left to try is marked `no_match`. One waiting on a provider that isn't connected stays queued until it is.

### The ladder

- **ISRC.** Several MusicBrainz recordings can share one ISRC. When exactly one is on a release from the listen's album date, it wins.
- **Release tracklist.** When the listen named a MusicBrainz release, that release's tracklist is read (one request, cached 30 minutes) and a track that reads like the listen is linked as an id-backed match.
- **Search.** Run only while nothing worth a candidate has been found: title, artist and album as exact phrases; then without the album and without feature or remaster tags; then, on MusicBrainz, looser word searches (artist + album, title + album, title + artist). Every result is pooled and scored the same way, so a looser query finds more but never links on less.
- **Corroboration.** Duration within 3 s and the album's release date confirm a match. Dates compare at the coarser precision (`2025` agrees with `2025-11-14`).
- **MusicBrainz track ids.** Some exports send a track id where a recording id belongs. When a held id isn't found as a recording, a track-id lookup resolves it and the real recording id is linked beside it.
- **Apple dates.** Apple reports a song's own date, not the album's, so the album date is fetched separately.

### Consent and candidates

Fuzzy matches are linked automatically only for recordings whose import gave fuzzy consent. Otherwise they are offered as candidates in **Admin → Review**. Exceptions are matches backed by an id: a track found on the release the listen named, or a lone candidate that reads exactly like the listen. Confirmed and automatic links are never rewritten by a later scan.

### High-precision mode

Off by default (`GET/PUT /api/v1/admin/matching`). For a recording with a Spotify or Apple id, the album's barcode finds the MusicBrainz release, and the track at the same disc and position is the match. It links only when exactly one release carries the barcode, durations agree, and ISRCs agree when both sides list them. Anything else falls back to the ladder. It applies to recordings matched from then on.

### Names in preferred languages

When the queue is idle (after an import, every 10 minutes, or when a language is added), the names pass:

- **Links artists across catalogues** using cached track answers. When two catalogues' credits for the same track leave exactly one unknown artist on each side, and one of them is ours by id, the other is linked to it.
- **Fetches names** for each preferred language: MusicBrainz aliases with that locale, Apple Music names from a storefront that supports the language, and Spotify names via `Accept-Language`. Each is stored per source in `entity_name`.

The display name follows the source order chosen under display naming: the first source with a name wins, in the first preferred language it has. A hand rename always wins.

## Review endpoints

All under `/api/v1/admin`, admin session required.

| Endpoint | Does |
|---|---|
| `GET /review` | Candidate links, newest first (`after_id`, `limit`) |
| `POST /review/links/{id}/confirm` | Confirm a candidate |
| `POST /review/links/{id}/reject` | Reject a candidate |
| `POST /review/recordings/{id}/rematch` | Queue a recording again |
| `POST /review/recordings/{id}/link` | Link a provider URL by hand, `{"url": ...}` |
| `GET /enrichment` | Queue counts by status |
| `POST /enrichment/requeue` | Queue every recording again |
| `GET`/`PUT /matching` | High-precision mode |
| `POST /connections/{provider}/check` | Test stored credentials, or `{"values": {...}}` without storing them |

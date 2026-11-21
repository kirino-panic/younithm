# API

Timestamps are RFC 3339 UTC strings (`2026-11-09T12:34:56Z`). Errors from `/api/v1` are JSON with an `error` code. Admin endpoints need a session cookie, and writes need the `X-CSRF-Token` header (see [security.md](security.md)).

Admin endpoints for specific features are documented with them: [imports](imports.md#api), [editing](editing.md#endpoints), [review and matching](providers.md#review-endpoints), [identity backfill](identity.md#backfill-and-integrity), [provider secrets](security.md#provider-secrets).

## Scrobbling (ListenBrainz-compatible)

Mounted at both `/1/...` and `/apis/listenbrainz/1/...`. Errors follow ListenBrainz: `{"code": 400, "error": "message"}`.

### `POST /1/submit-listens`

Header `Authorization: Token <token>` (unless the admin turned the token off).

```json
{
  "listen_type": "single | import | playing_now",
  "payload": [{
    "listened_at": 1700000000,
    "track_metadata": {
      "artist_name": "required",
      "track_name": "required",
      "release_name": "optional",
      "additional_info": {
        "recording_mbid": "", "release_mbid": "", "artist_mbids": [],
        "artist_names": [], "spotify_id": "", "isrc": "",
        "duration_ms": 0, "submission_client": ""
      }
    }
  }]
}
```

- `single` and `playing_now` take exactly one item; `import` takes up to 1000.
- `playing_now` must not carry `listened_at`; the others need it (integer seconds, after 2002-09-30, at most 24 h in the future).
- Body up to 10 MiB. The whole payload is kept in `scrobble.raw_payload`, and every id the resolver understands is used (see [identity.md](identity.md)).
- Success: `200 {"status": "ok", "accepted": N, "duplicates": M}`. `401` for a missing or wrong token.

### `GET /1/validate-token`

`{"code": 200, "message": "Token valid.", "valid": true, "user_name": "..."}`, or `"valid": false`.

## Public read API

Each is subject to its visibility toggle and the per-IP rate limit.

| Endpoint | Returns |
|---|---|
| `GET /api/v1/now-playing` | `{"playing": false}` or `{"playing": true, "track": {...}, "source", "started_at", "expires_at", "progress_ms"}` |
| `GET /api/v1/now-playing/stream` | Server-sent events: `now-playing` with the same JSON on connect and on every change; a ping every 15 s |
| `GET /api/v1/recent` | Latest listens (`limit`) |
| `GET /api/v1/top/{kind}` | Top `artists`, `tracks` or `albums` (`range`, `limit`) |
| `GET /api/v1/stats` | Totals for a range (`range`) |
| `GET /api/v1/scrobbles` | Scrobble list, newest first (`limit` 1–200, `cursor`, `q`, `from`, `to`, `source`); returns `items`, `next_cursor`, `total` |
| `GET /api/v1/artists/{id}`, `/releases/{id}`, `/recordings/{id}` | Entity pages |
| `GET /api/v1/recordings/{id}/history` | Every play of a recording |
| `GET /api/v1/library/{kind}` | Browse `artists`, `albums` or `tracks` (`sort` = `plays`, `name` or `last`; `q`, `limit`, `offset`) |
| `GET /api/v1/art/{kind}/{id}` | Artwork image |
| `GET /healthz` | Health check |

`range` is `all` (default), `week`, `month`, `year` or `latest_week` (the seven days up to the newest listen).

A now-playing entry expires at its start plus the track's duration (plus 10 s), or after 5 minutes when no duration was sent. It clears early when a `single` listen of the same track arrives.

## Site

| Endpoint | Auth | Does |
|---|---|---|
| `GET /api/v1/site` | Public | Section visibility |
| `GET /api/v1/site/page` | Public | Home page document (theme, tiles, custom CSS), stored as opaque JSON |
| `GET /api/v1/site/profile`, `/site/profile/avatar` | Public | Display name and avatar |
| `PUT /api/v1/admin/site/page` | Session | Save the home page |
| `PUT /api/v1/admin/profile` | Session | Save display name and avatar |
| `GET /api/v1/admin/settings` | Session | Sections with their public flags |
| `PUT /api/v1/admin/settings/visibility` | Session | `{"sections": {"scrobbles": false}}` |
| `GET`/`PUT /api/v1/admin/scrobbling` | Session (+ step-up to change) | Scrobble token: `{"require_token", "token"}` |
| `GET /api/v1/admin/onboarding`, `POST .../onboarding/done` | Session | Whether the welcome flow is finished |
| `GET`/`PUT /api/v1/admin/naming` | Session | Display-name source order |
| `GET`/`PUT /api/v1/admin/languages` | Session | Preferred languages |

## Auth

| Endpoint | Auth | Does |
|---|---|---|
| `GET /api/v1/setup/status` | None | `{"setup_required"}` |
| `POST /api/v1/setup` | None | `{username, password}`: create the admin and sign in (only while none exists) |
| `GET /api/v1/auth/me` | None | `{authenticated, username, csrf, step_up}`, or `{authenticated: false, setup_required}` |
| `POST /api/v1/auth/login` | None | `{username, password}` |
| `POST /api/v1/auth/logout` | Session | Sign out |
| `POST /api/v1/auth/reauth` | Session | `{password}`: open the 5-minute step-up window |
| `POST /api/v1/auth/password` | Session | `{current, new, sign_out_others}` |
| `POST /api/v1/auth/username` | Session | `{current, username}` |

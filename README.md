# younithm

A self-hosted scrobble server. Scrobblers send listens to it through a ListenBrainz-compatible API, you import your history from other services, and it links every listen to artists, tracks and albums using MusicBrainz, Spotify and Apple Music. It serves a public page of what you listen to, and an admin area for editing and imports.

One Go binary with the web frontend embedded, one SQLite database, one Docker image.

## Run it

```bash
docker compose up -d
```

Open `http://localhost:2015`. The first visitor creates the admin account, then a short welcome flow sets up your profile, connections and first import.

All state lives in the `/config` volume (`./data` in the compose file): the database, the master key, artwork and backups. Back up `master.key` on its own; without it, stored provider keys can't be decrypted.

## Configuration

Everything is optional.

| Variable | Default | Purpose |
|---|---|---|
| `YOUNITHM_ADDR` | `:2015` | Listen address |
| `YOUNITHM_BASE_PATH` | empty | Serve under a path such as `/younithm` instead of a whole domain |
| `YOUNITHM_CONFIG_DIR` | `/config` | Database, master key, artwork, uploads, backups |
| `YOUNITHM_TRUSTED_PROXIES` | empty | Reverse proxy IPs/CIDRs whose `X-Forwarded-*` headers are trusted |
| `YOUNITHM_TRUSTED_PROXY` | `false` | Trust `X-Forwarded-*` from any peer (prefer the list above) |
| `YOUNITHM_MASTER_KEY` | unset | Master key, 32 bytes as base64 or hex |
| `YOUNITHM_MASTER_KEY_FILE` | unset | File holding the master key (e.g. a Docker secret) |
| `MUSICBRAINZ_CONTACT` | empty | Contact for the MusicBrainz User-Agent; can also be set in Connections |
| `YOUNITHM_ENABLE_ITUNES` | `false` | Keyless iTunes Search fallback while no Apple Music keys are set |
| `YOUNITHM_DEDUPE_WINDOW` | `2s` | Live scrobbles closer than this, same artist and title, count once |
| `YOUNITHM_IMPORT_DEDUPE_WINDOW` | `30s` | The same tolerance for imports |
| `YOUNITHM_IMPORT_MAX_BYTES` | 1 GiB | Largest accepted import file |
| `PUID` / `PGID` | `99` / `100` | Container user (Unraid defaults) |

Development switches are in [docs/development.md](docs/development.md).

## Connect a scrobbler

Point any ListenBrainz client (Multi-Scrobbler, Web Scrobbler, Navidrome and others) at `http://<host>:2015` (or `/apis/listenbrainz`) and use the token you chose under **Admin → Connections**. See [docs/api.md](docs/api.md).

## Documentation

| | |
|---|---|
| [Architecture](docs/architecture.md) | How the pieces fit, the data model, background workers |
| [Identity](docs/identity.md) | How a listen becomes artists, a track and an album |
| [Providers](docs/providers.md) | MusicBrainz, Spotify, Apple Music and the enrichment worker |
| [Imports](docs/imports.md) | Supported formats, the import lifecycle, dedupe |
| [Editing](docs/editing.md) | Editing, merging, aliases and undo |
| [Artwork](docs/artwork.md) | Covers and artist images |
| [API](docs/api.md) | Scrobbler, public and auth endpoints |
| [Security](docs/security.md) | Admin auth, the scrobble token, secrets, proxies |
| [Deployment](docs/deployment.md) | Docker, Unraid, reverse proxies, backups, password reset |
| [Development](docs/development.md) | Building, running locally, tests, debug tools |

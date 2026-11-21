# Development

Needs Go (version in `go.mod`) and Node 24.

## Run locally

```bash
make dev-api     # Go server on :2015, data in ./data
```

```bash
make dev-web     # Vite on :5173, proxying /api and /1 to :2015
```

Open `http://localhost:5173`. The Go binary embeds `web/dist`; without a frontend build it still compiles (an empty `dist/.gitkeep` keeps `go:embed` happy) but serves no UI.

| Target | Does |
|---|---|
| `make dev-api` | Run the server against `./data` |
| `make dev-debug` | The same with the debug tools and debug UI on, against `./data-debug` |
| `make dev-web` | Frontend dev server |
| `make web` | Build the frontend into `web/dist` |
| `make build` | Frontend, then a static binary in `bin/younithm` |
| `make run` | Build and run against `./data` |
| `make docker` | Build the image `younithm:dev` |

## Tests

```bash
go test ./...
```

```bash
cd web && npm run typecheck
```

## Debug tools

| Variable | Turns on |
|---|---|
| `YOUNITHM_DEBUG=1` | A **Debug** tab in Admin → Settings: library counts, a fake now-playing track, an import size limit, clearing the library, and **Simulate onboarding** (a dry run of the welcome flow with a panel to jump between screens, and force failures) |
| `YOUNITHM_DEBUG_UI=1` | The developer UI at `/debug/` |
| `YOUNITHM_DEBUG_IMPORT_LIMIT=N` | Import only the newest N listens of each file |

### Developer UI

A single page with no build step, for working on the backend directly: connect providers and test them, import listens, see counts, run the full matching pipeline (identity re-resolve, then provider matching), and open any artist, recording, release or scrobble to see its raw row and everything attached to it (links, aliases, snapshots, merge candidates, edit log).

It has **no login** and refuses any request that isn't a direct loopback connection, so it doesn't work inside Docker or behind a proxy, by design. Code: `internal/debugui`.

## Conventions

- **Migrations** are numbered goose files in `internal/db/migrations`, embedded and applied at startup. Add a new file for every schema change; never edit one that has shipped.
- **Database access.** Writes and write transactions go through `Pools.Write` (a single connection); reads through `Pools.Read`.
- **Raw values are never overwritten.** Anything a scrobbler or file submitted stays on the scrobble; corrections go on the entities.
- **No guessing.** The resolver and matcher stop at a candidate rather than merge on weak evidence. See [identity.md](identity.md).
- **Providers are stateless.** Clients in `internal/providers/*` never touch the database; caching and linking sit above them.

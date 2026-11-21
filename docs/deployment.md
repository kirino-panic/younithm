# Deployment

## Docker

The image is built from the repository's `Dockerfile` (frontend, then a static Go binary, on Alpine).

```bash
docker compose up -d
```

- **Volume.** Everything lives in `/config`: `younithm.db`, `master.key`, `artwork/`, `imports/` (uploads in progress) and `backups/`.
- **User.** The entrypoint takes ownership of `/config` and drops to `PUID`:`PGID` (default `99:100`, Unraid's `nobody:users`).
- **Health check.** `GET /healthz`, polled every 30 s by the image.
- **Upgrades** migrate the database at startup, after writing a copy to `/config/backups/` (the newest five are kept).

## Unraid

Map `/config` to `/mnt/user/appdata/younithm`. SQLite runs in WAL mode, so keep appdata on the cache pool and, ideally, map it through `/mnt/cache/...` rather than the `/mnt/user` FUSE layer.

## Reverse proxy

Put the app behind Nginx Proxy Manager, SWAG, Caddy, Cloudflare Tunnel or similar, and tell younithm which peers are proxies:

```yaml
environment:
  YOUNITHM_TRUSTED_PROXIES: "172.18.0.0/16"
```

This makes cookies `Secure` when the proxy terminates HTTPS, and lets login throttling see the real client IP. Details in [security.md](security.md#reverse-proxies).

Scrobblers that expect the ListenBrainz path layout can use `https://<host>/apis/listenbrainz`.

### Under a path

To run at `https://example.com/younithm/` instead of a whole (sub)domain, set the path and route it to the container:

```yaml
environment:
  YOUNITHM_BASE_PATH: /younithm
```

```
example.com {
    handle /younithm/* {
        reverse_proxy younithm:2015
    }
}
```

The proxy may forward the path as is or strip the prefix; both work. The image stays the same, and `/healthz` still answers at the root for the health check. Scrobblers then use `https://example.com/younithm/apis/listenbrainz`.

## Backups

Back up the `/config` volume. Keep a copy of `master.key` somewhere separate: database backups are useless without it for provider secrets, which is the point.

## Lost admin password

See [security.md](security.md#lost-password).

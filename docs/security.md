# Security

younithm has one admin account. Everything public is read-only.

## Admin account

- **First run.** While no admin exists, the first visitor to `/setup` creates the account. Setup refuses once an admin exists.
- **Passwords** are hashed with Argon2id (64 MiB, t=3, p=2), 10 to 256 characters. Unknown usernames take the same time to reject as wrong passwords.
- **Sessions** are server-side: a random 256-bit token in the cookie, only its SHA-256 stored. Cookies are `HttpOnly` and `SameSite=Strict`, and `Secure` when the request arrived over TLS or a trusted proxy says it did. Sessions last 30 days, or 7 days idle.
- **CSRF.** Every unsafe request with a session must carry the session's token in `X-CSRF-Token`. Clients read it from `/api/v1/auth/me` or the login response.
- **Login throttling.** Per-IP and per-account counters in SQLite: 5 free failures, then a lock that starts at 30 s and doubles, up to 15 minutes. Locked attempts get `429` with `Retry-After`. A correct password doesn't bypass an active lock.
- **Step-up.** Sensitive actions (provider keys, the scrobble token, merges, bulk edits, confirming imports, clearing the library) need the password again within the last 5 minutes (`POST /api/v1/auth/reauth`). Without it they return `403 step_up_required`.
- **Password change** signs out other sessions on request.

### Lost password

Run `reset-admin` against the config directory. Access to that directory is the credential.

```bash
echo 'a new long password' | younithm reset-admin --stdin
```

```bash
docker exec -i -u 99:100 younithm sh -c "younithm reset-admin --stdin" <<< 'a new long password'
```

It sets the password, signs out every session and clears lockouts. The password is never accepted as a command-line argument.

## Scrobble token

The ListenBrainz endpoints accept one token, chosen by the admin under **Admin → Connections** and stored as a SHA-256 hash. The admin can also turn the token off, so any client that knows the address can submit. Until the admin decides, a token is required. The token only reaches the submit and validate endpoints, never admin functions.

## Provider secrets

Spotify and Apple Music credentials and the MusicBrainz contact are stored encrypted in `provider_secret`. Known names: `spotify.client_id`, `spotify.client_secret`, `apple.team_id`, `apple.key_id`, `apple.private_key`, `musicbrainz.contact`.

- **Write-only.** The API returns whether a value is set and, for values of 12 characters or more, its last 4 characters. Values are never returned.
- **Encryption.** AES-256-GCM. The ciphertext is `ys1:` + base64url(key id ‖ nonce ‖ ciphertext), and the additional data binds each value to its name, so values can't be swapped between rows.
- **Redaction.** Every stored value is registered with a redactor that replaces it with `[redacted]` in log output.

| Endpoint | Guard |
|---|---|
| `GET /api/v1/admin/secrets` | Session |
| `PUT /api/v1/admin/secrets/{name}` `{"value": ...}` | Session + step-up |
| `DELETE /api/v1/admin/secrets/{name}` | Session + step-up |

### Master key

The key is resolved in this order: `YOUNITHM_MASTER_KEY` (32 bytes, base64 or hex), `YOUNITHM_MASTER_KEY_FILE`, then `<config>/master.key`. If none exists, a random key is written to `master.key` (mode `0600`) on first run.

- Losing the key loses every stored secret. Back it up separately from database backups; a backup without the key reveals nothing.
- A configured key that is malformed or unreadable, or a corrupt `master.key`, stops startup. A replacement is never generated silently.
- The key protects a leaked database or backup, not a compromised host: by default it sits next to the database. To keep it off the volume, pass it with `YOUNITHM_MASTER_KEY_FILE` pointing at a Docker secret.

## Reverse proxies

Forwarded headers are ignored unless the operator opts in:

- `YOUNITHM_TRUSTED_PROXIES`: comma-separated IPs or CIDRs. `X-Forwarded-Proto` and `X-Forwarded-For` are honoured only when the direct peer is in the list.
- `YOUNITHM_TRUSTED_PROXY=true`: honour them from any peer. Only safe when the app port is reachable solely through the proxy. A warning is logged at startup.

Behind a trusted proxy, the client IP is found by walking `X-Forwarded-For` from the right, skipping trusted addresses. Entries further left are client-controlled and never believed.

## Public surface

- Public read endpoints are rate limited per IP (2 requests per second, bursts of 30). Admin sessions are exempt. Artwork is not limited.
- Each public section (scrobble list, now playing, recent listens, top lists, stats, entity and library pages) has a visibility toggle; all default to public. A hidden section is still visible to the admin.
- Edit history, raw values, import jobs and settings are admin-only.

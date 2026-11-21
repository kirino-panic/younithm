# Artwork

Album covers and artist images are downloaded once from the providers an entity is linked to and kept in `<config>/artwork`. Code: `internal/artwork`.

- **Worker.** Every 30 s it takes up to 25 releases and artists that have an accepted provider link and no image, most-played first. It asks Spotify, then Apple Music, then MusicBrainz (Cover Art Archive). Artist images come from Spotify and Apple only.
- **Never overwrites.** A fetched image is stored only while the entity has none.
- **Overrides.** An image uploaded in the editor is stored separately and always shown instead of the fetched one. Resetting it brings the fetched image back.
- **Retries.** "No image anywhere" is retried after 14 days; a failed call or download after an hour. A rate-limit answer ends the pass.
- **Files** are named `<kind>-<id>-<hash>.<ext>`. Only JPEG, PNG, WebP and GIF are kept (checked by content), up to 8 MB. Downloads are HTTPS only and refuse non-public addresses.
- **Serving.** `GET /api/v1/art/{kind}/{id}` (`release` or `artist`) is public and not rate limited. API responses carry an `image` URL with the hash as `?v=`, so browsers can cache it forever.

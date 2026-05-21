# ballcoach-media

BallCoach Media API service for storing and serving avatars, exercise images, and meditation audio tracks (MP3/M4A/AAC). Storage backend: **Cloudflare R2**.

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/avatars` | Upload avatar image (`file`, `filename`). Response `full_url` is the R2 public URL (served via Cloudflare CDN). |
| `POST` | `/exercise-images` | Upload exercise image. Response `full_url` is the R2 public URL. |
| `POST` | `/meditation-tracks` | Upload meditation audio (MP3/M4A/AAC). Response `full_url` points at the proxy stream endpoint. |
| `GET` | `/avatars/{filename}` | 302-redirect to R2 public URL (legacy compat). |
| `GET` | `/exercise-images/{filename}` | 302-redirect to R2 public URL (legacy compat). |
| `GET` | `/meditation-tracks/{filename}/stream` | Proxy stream from R2 with HTTP Range support. |
| `DELETE` | `/avatars/{filename}` | Delete avatar from R2. |
| `DELETE` | `/exercise-images/{filename}` | Delete exercise image from R2. |
| `DELETE` | `/meditation-tracks/{filename}` | Delete meditation track from R2. |
| `POST` | `/admin/migrate-to-r2` | One-shot migration: walks the legacy Railway volume and uploads everything to R2. Idempotent. |
| `GET` | `/health` | Health check. |

## Storage Layout (R2 keys)

```
avatars/<filename>
exercise-images/<filename>
meditation-tracks/<filename>
```

Images are served directly from R2's public domain (`R2_PUBLIC_BASE_URL`) and benefit from Cloudflare's CDN. Audio is proxied through this service to preserve range-stream behavior.

## Validation Rules

- Avatar / exercise images: max 10 MB. JPEG, PNG, WebP, HEIC.
- Meditation tracks: max 50 MB. MP3, M4A, AAC. Duration is only calculated for MP3.

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `PORT` | no (`3001`) | HTTP listen port |
| `R2_ACCOUNT_ID` | **yes** | Cloudflare account ID (32 hex chars) |
| `R2_ACCESS_KEY_ID` | **yes** | R2 API token access key |
| `R2_SECRET_ACCESS_KEY` | **yes** | R2 API token secret |
| `R2_BUCKET` | **yes** | R2 bucket name (e.g. `ballcoach-media`) |
| `R2_PUBLIC_BASE_URL` | **yes** | Public base URL of the bucket (`https://pub-<hash>.r2.dev` or custom domain). No trailing slash. |
| `MEDIA_API_BASE_URL` | no | Used to build `full_url` for audio proxy responses |
| `MEDIA_STORAGE_PATH` | no (`/data/media`) | Only used during one-shot migration from the Railway volume |
| `CORS_ORIGINS` | no | Comma-separated origins for `POST`/`DELETE` CORS |

## Local Development

```bash
export R2_ACCOUNT_ID=...
export R2_ACCESS_KEY_ID=...
export R2_SECRET_ACCESS_KEY=...
export R2_BUCKET=ballcoach-media
export R2_PUBLIC_BASE_URL=https://pub-xxx.r2.dev
go run .
```

## Example Upload

```bash
curl -X POST http://localhost:3001/avatars \
  -F "file=@./avatar.jpg" \
  -F "filename=user123-1712937600.jpg"
```

## Migrating from the Railway Volume to R2

After the new code is deployed with R2 env vars set, run once:

```bash
curl -X POST https://<your-api>/admin/migrate-to-r2
```

The endpoint walks the legacy volume layout (`MEDIA_STORAGE_PATH/{avatars,exercise-images,meditation-tracks}` and the direct `/data/meditation-audio` mount) and uploads each file to R2 with key `<dir>/<filename>`. Already-present keys are skipped, so the call is safe to retry.

Once the response shows `failed: []` and the file counts look right, you can detach the Railway volume.

# ballcoach-media

Minimal Go microservice that serves media files (profile photos, audio) from a mounted volume. Built for Railway.

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Returns `{"status":"ok"}` |
| GET | `/media/{filename}` | Serves the requested file with correct MIME type |

Supported formats: JPEG, PNG, WebP, MP3, MP4, AAC, M4A.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP listen port (Railway sets this automatically) |
| `MEDIA_DIR` | `/data` | Base directory where media files are stored |

## Railway Setup

1. Create a new project and link this repo.
2. Add a **Volume** to the service:
   - Mount path: `/data`
   - This is where uploaded media files will live.
3. Railway will build the Dockerfile automatically. No extra env vars needed unless you want a custom `MEDIA_DIR`.

## Local Development

```bash
go run .                           # serves from /data on :8080
MEDIA_DIR=./testdata go run .      # serves from a local directory
```

## Example Request

```
GET https://your-service.up.railway.app/media/avatar-abc123.jpg
```

Response: the image bytes with `Content-Type: image/jpeg` and `Access-Control-Allow-Origin: *`.

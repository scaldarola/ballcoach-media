# ballcoach-media

BallCoach Media API service for storing and serving avatars, exercise images, and meditation audio tracks (MP3/M4A/AAC) from persistent filesystem storage.

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/avatars` | Upload avatar image (`file`, `filename`) |
| `POST` | `/exercise-images` | Upload exercise image (`file`, `filename`) |
| `POST` | `/meditation-tracks` | Upload meditation audio (`file`, `filename`) - supports MP3/M4A/AAC |
| `GET` | `/avatars/{filename}` | Serve avatar image |
| `GET` | `/exercise-images/{filename}` | Serve exercise image |
| `GET` | `/meditation-tracks/{filename}/stream` | Stream meditation audio with range support |
| `DELETE` | `/avatars/{filename}` | Delete avatar |
| `DELETE` | `/exercise-images/{filename}` | Delete exercise image |
| `DELETE` | `/meditation-tracks/{filename}` | Delete meditation track |
| `GET` | `/health` | Health + storage/disk check |

## Storage Layout

```
MEDIA_STORAGE_PATH/
├── avatars/
├── exercise-images/
└── meditation-tracks/
```

## Validation Rules

- Avatar and exercise images:
  - Max `10MB`
  - Allowed MIME/types: JPEG, PNG, WebP, HEIC
  - Required multipart fields: `file`, `filename`
- Meditation tracks:
  - Max `50MB`
  - Allowed formats: MP3 (`.mp3`), M4A (`.m4a`), AAC (`.aac`)
  - Allowed MIME types: `audio/mpeg`, `audio/mp4`, `audio/aac`, `audio/x-m4a`, `audio/m4a`
  - Required multipart fields: `file`, `filename`
  - Note: Duration calculation only supported for MP3 files. M4A/AAC files return `duration_seconds: 0`

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `3001` | HTTP listen port |
| `MEDIA_STORAGE_PATH` | `/data/media` | Base storage directory |
| `MEDIA_API_BASE_URL` | unset | Used to build `full_url` upload response field |
| `CORS_ORIGINS` | unset | Comma-separated origins for `POST`/`DELETE` CORS |

## Local Development

```bash
go run .
```

With custom storage path:

```bash
MEDIA_STORAGE_PATH=./data go run .
```

## Example Upload

```bash
curl -X POST http://localhost:3001/avatars \
  -F "file=@./avatar.jpg" \
  -F "filename=user123-1712937600.jpg"
```

## Migration: MP3 to M4A

If you have existing MP3 files and want to convert them to M4A for better iOS compatibility, see [MIGRATION.md](./MIGRATION.md) for detailed instructions.

Quick command:
```bash
go run migrate_mp3_to_m4a.go /data/media
```

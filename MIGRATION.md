# MP3 to M4A Migration Guide

This guide explains how to convert existing MP3 meditation tracks to M4A format for better iOS compatibility.

## Why M4A?

- **Better iOS support**: M4A/AAC format has superior compatibility with iOS devices and AVPlayer
- **Smaller file sizes**: AAC codec provides better compression than MP3 at similar quality
- **Faster loading**: iOS handles M4A files more efficiently

## Prerequisites

The Dockerfile has been updated to include `ffmpeg`, which is required for audio conversion.

## Running the Migration on Railway

### Option 1: Using Railway CLI (Recommended)

1. **Install Railway CLI** (if not already installed):
   ```bash
   npm i -g @railway/cli
   ```

2. **Login to Railway**:
   ```bash
   railway login
   ```

3. **Link to your project**:
   ```bash
   railway link
   ```

4. **Deploy the updated Dockerfile** (with ffmpeg):
   ```bash
   git add .
   git commit -m "feat: add ffmpeg for audio conversion"
   git push
   ```
   Wait for deployment to complete.

5. **Run the migration script**:
   ```bash
   railway run go run migrate_mp3_to_m4a.go
   ```

### Option 2: Using Railway Shell

1. **Deploy the updated code** with ffmpeg in Dockerfile

2. **Open Railway dashboard** → Select your service → **Shell** tab

3. **Download the migration script** in the shell:
   ```bash
   # The file should already be in the deployed container
   ls -la migrate_mp3_to_m4a.go
   ```

4. **Run the migration**:
   ```bash
   go run migrate_mp3_to_m4a.go /data/media
   ```

### Option 3: Create a Temporary Endpoint

If shell access is difficult, you can add a temporary admin endpoint:

```go
// Add to main.go temporarily
r.Post("/admin/migrate-to-m4a", func(w http.ResponseWriter, r *http.Request) {
    // Run migration inline
    // ... (copy logic from migrate_mp3_to_m4a.go)
})
```

Then call it via:
```bash
curl -X POST https://your-app.railway.app/admin/migrate-to-m4a
```

**⚠️ Remember to remove this endpoint after migration!**

## What the Script Does

1. Scans `/data/media/meditation-tracks` for MP3 files
2. Converts each MP3 to M4A using ffmpeg with:
   - AAC codec
   - 128kbps bitrate (good quality for speech/meditation)
   - Preserves original MP3 files
3. Reports conversion success/failure

## After Migration

1. **Verify M4A files work** by testing audio playback in the app
2. **Keep both formats temporarily** for rollback capability
3. **Delete MP3 files** once you confirm M4A works perfectly:
   ```bash
   # On Railway shell
   rm /data/media/meditation-tracks/*.mp3
   ```

## Troubleshooting

### "ffmpeg not found"
- Make sure you deployed the updated Dockerfile with ffmpeg
- Redeploy if needed: `git push`

### "Permission denied"
- Check volume permissions: `ls -la /data/media/meditation-tracks`
- Railway should have write access to the volume

### "Directory does not exist"
- Verify storage path: `echo $MEDIA_STORAGE_PATH`
- Try different path: `go run migrate_mp3_to_m4a.go /path/to/media`

## Example Output

```
🔍 Scanning for MP3 files in: /data/media/meditation-tracks
📝 Found 3 MP3 file(s) to convert

[1/3] Converting: meditation-calm-1.mp3
   ✅ Created: meditation-calm-1.m4a (3.45 MB)

[2/3] Converting: meditation-focus-2.mp3
   ✅ Created: meditation-focus-2.m4a (4.12 MB)

[3/3] Converting: meditation-relax-3.mp3
   ✅ Created: meditation-relax-3.m4a (2.98 MB)

============================================================
✅ Migration complete!
   Converted: 3
   Failed: 0
   Total: 3
============================================================

📝 Note: Original MP3 files have been preserved.
   You can delete them manually after verifying the M4A files work correctly.
```

## Rollback

If you need to rollback:

1. Keep the MP3 files (script preserves them by default)
2. Delete M4A files if needed
3. Revert code changes if necessary

The API supports both formats, so you can keep both during testing.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/tcolgate/mp3"
)

const (
	maxImageSize = 10 * 1024 * 1024
	maxAudioSize = 50 * 1024 * 1024
)

type uploadConfig struct {
	keyPrefix    string
	maxSize      int64
	allowedMIMEs map[string]struct{}
	allowedExts  map[string]struct{}
	// publicMode=true → respond with the R2 public URL (used for images served via CDN).
	// publicMode=false → respond with our proxy URL (used for audio that needs range-stream proxying).
	publicMode  bool
	streamRoute string // route template used when publicMode=false, e.g. "/meditation-tracks/%s/stream"
}

type errorResponse struct {
	Error string `json:"error"`
}

type uploadResponse struct {
	URL             string `json:"url"`
	FullURL         string `json:"full_url"`
	Filename        string `json:"filename"`
	SizeBytes       int64  `json:"size_bytes"`
	DurationSeconds int64  `json:"duration_seconds,omitempty"`
}

type originConfig struct {
	allowAnyGet bool
	origins     map[string]struct{}
}

type ipRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*visitor
	limit    int
	window   time.Duration
	ttl      time.Duration
}

type visitor struct {
	count      int
	windowFrom time.Time
	lastAccess time.Time
}

func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})

	port := os.Getenv("PORT")
	if port == "" {
		port = "3001"
	}

	baseURL := os.Getenv("MEDIA_API_BASE_URL")
	legacyStoragePath := os.Getenv("MEDIA_STORAGE_PATH")
	if legacyStoragePath == "" {
		legacyStoragePath = "/data/media"
	}

	r2Cfg, err := loadR2ConfigFromEnv()
	if err != nil {
		log.Fatal().Err(err).Msg("R2 not configured")
	}
	ctx := context.Background()
	r2c, err := newR2Client(ctx, r2Cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init R2 client")
	}

	registerMIMETypes()
	origins := parseOrigins(os.Getenv("CORS_ORIGINS"))
	limiter := newIPRateLimiter(20, 10*time.Second, 10*time.Minute)

	r := chi.NewRouter()
	r.Use(requestLogger)
	r.Use(corsMiddleware(origins))
	r.Use(uploadRateLimitMiddleware(limiter))

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"backend": "r2",
			"bucket":  r2Cfg.bucket,
		})
	})

	// One-shot migration: copies everything under the legacy Railway volume
	// into R2 with key `<dir>/<filename>`. Idempotent (HeadObject check).
	r.Post("/admin/migrate-to-r2", func(w http.ResponseWriter, req *http.Request) {
		result := migrateVolumeToR2(req.Context(), r2c, legacyStoragePath)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})

	r.Post("/avatars", uploadHandler(r2c, baseURL, uploadConfig{
		keyPrefix:    "avatars",
		maxSize:      maxImageSize,
		allowedMIMEs: set("image/jpeg", "image/png", "image/webp", "image/heic"),
		allowedExts:  set(".jpg", ".jpeg", ".png", ".webp", ".heic"),
		publicMode:   true,
	}, false))

	r.Post("/exercise-images", uploadHandler(r2c, baseURL, uploadConfig{
		keyPrefix:    "exercise-images",
		maxSize:      maxImageSize,
		allowedMIMEs: set("image/jpeg", "image/png", "image/webp", "image/heic"),
		allowedExts:  set(".jpg", ".jpeg", ".png", ".webp", ".heic"),
		publicMode:   true,
	}, false))

	r.Post("/meditation-tracks", uploadHandler(r2c, baseURL, uploadConfig{
		keyPrefix:    "meditation-tracks",
		maxSize:      maxAudioSize,
		allowedMIMEs: set("audio/mpeg", "audio/mp4", "audio/aac", "audio/x-m4a", "audio/m4a"),
		allowedExts:  set(".mp3", ".m4a", ".aac"),
		publicMode:   false,
		streamRoute:  "/meditation-tracks/%s/stream",
	}, true))

	// Image serve endpoints: redirect to the R2 public URL so clients that still
	// hit /avatars/{f} keep working. New clients should use the FullURL from upload.
	r.Get("/avatars/{filename}", redirectToPublicHandler(r2c, "avatars"))
	r.Get("/exercise-images/{filename}", redirectToPublicHandler(r2c, "exercise-images"))

	// Audio serve endpoint: proxy from R2 with Range support.
	r.Get("/meditation-tracks/{filename}/stream", proxyR2Handler(r2c, "meditation-tracks", 7*24*time.Hour))

	r.Delete("/avatars/{filename}", deleteFileHandler(r2c, "avatars"))
	r.Delete("/exercise-images/{filename}", deleteFileHandler(r2c, "exercise-images"))
	r.Delete("/meditation-tracks/{filename}", deleteFileHandler(r2c, "meditation-tracks"))

	log.Info().Str("port", port).Str("backend", "r2").Str("bucket", r2Cfg.bucket).Msg("ballcoach-media listening")
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatal().Err(err).Msg("server failed")
	}
}

func uploadHandler(r2c *r2Client, baseURL string, cfg uploadConfig, includeDuration bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, cfg.maxSize+(1*1024*1024))
		if err := r.ParseMultipartForm(cfg.maxSize + (1 * 1024 * 1024)); err != nil {
			writeJSONError(w, http.StatusBadRequest, "file too large")
			return
		}

		filename := strings.TrimSpace(r.FormValue("filename"))
		if filename == "" {
			writeJSONError(w, http.StatusBadRequest, "missing filename")
			return
		}
		if !isValidFilename(filename) {
			writeJSONError(w, http.StatusBadRequest, "invalid filename")
			return
		}

		file, fileHeader, err := r.FormFile("file")
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "missing file")
			return
		}
		defer file.Close()

		if fileHeader.Size > cfg.maxSize {
			writeJSONError(w, http.StatusBadRequest, "file too large")
			return
		}

		if !isAllowedType(file, fileHeader, filename, cfg) {
			writeJSONError(w, http.StatusBadRequest, "invalid file type")
			return
		}

		// Buffer the upload to a local temp file so we can both compute audio
		// duration (which needs random access) and stream it to R2 with a known
		// Content-Length.
		tmp, err := os.CreateTemp("", "ballcoach-upload-*")
		if err != nil {
			log.Error().Err(err).Msg("failed to create temp file")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		tmpPath := tmp.Name()
		defer func() {
			_ = os.Remove(tmpPath)
		}()

		written, copyErr := io.Copy(tmp, io.LimitReader(file, cfg.maxSize+1))
		closeErr := tmp.Close()
		if copyErr != nil || closeErr != nil {
			log.Error().Err(firstErr(copyErr, closeErr)).Msg("failed to buffer upload")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if written > cfg.maxSize {
			writeJSONError(w, http.StatusBadRequest, "file too large")
			return
		}

		key := cfg.keyPrefix + "/" + filename
		contentType := contentTypeForFilename(filename)

		uploadFile, err := os.Open(tmpPath)
		if err != nil {
			log.Error().Err(err).Msg("failed to reopen temp file")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		putErr := r2c.put(r.Context(), key, contentType, uploadFile, written)
		_ = uploadFile.Close()
		if putErr != nil {
			log.Error().Err(putErr).Str("key", key).Msg("failed to put object to R2")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		resp := uploadResponse{
			Filename:  filename,
			SizeBytes: written,
		}

		if cfg.publicMode {
			publicURL := r2c.publicURL(key)
			resp.URL = "/" + key
			resp.FullURL = publicURL
		} else {
			route := fmt.Sprintf(cfg.streamRoute, filename)
			resp.URL = route
			resp.FullURL = resolveFullURL(baseURL, r, route)
		}

		if includeDuration {
			durationSeconds, err := calculateAudioDurationSeconds(tmpPath, filename)
			if err != nil {
				log.Warn().Err(err).Str("filename", filename).Msg("failed to calculate audio duration")
			}
			resp.DurationSeconds = durationSeconds
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// redirectToPublicHandler 302-redirects to the public R2 URL. Used for images so
// legacy clients hitting /avatars/{f} keep working while the file is actually
// served by Cloudflare's CDN.
func redirectToPublicHandler(r2c *r2Client, prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filename := chi.URLParam(r, "filename")
		if !isValidFilename(filename) {
			http.NotFound(w, r)
			return
		}
		key := prefix + "/" + filename
		w.Header().Set("Cache-Control", "public, max-age=300")
		http.Redirect(w, r, r2c.publicURL(key), http.StatusFound)
	}
}

func proxyR2Handler(r2c *r2Client, prefix string, cacheFor time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filename := chi.URLParam(r, "filename")
		if !isValidFilename(filename) {
			http.NotFound(w, r)
			return
		}
		key := prefix + "/" + filename
		r2c.proxyObject(r.Context(), w, r, key, cacheFor)
	}
}

func deleteFileHandler(r2c *r2Client, prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filename := chi.URLParam(r, "filename")
		if !isValidFilename(filename) {
			writeJSONError(w, http.StatusNotFound, "file not found")
			return
		}
		key := prefix + "/" + filename

		if _, err := r2c.head(r.Context(), key); err != nil {
			if isR2NotFound(err) {
				writeJSONError(w, http.StatusNotFound, "file not found")
				return
			}
			log.Error().Err(err).Str("key", key).Msg("failed to head object")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if err := r2c.delete(r.Context(), key); err != nil {
			log.Error().Err(err).Str("key", key).Msg("failed to delete object")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(ww, r)
		log.Info().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", ww.status).
			Dur("latency", time.Since(start)).
			Str("remote_addr", r.RemoteAddr).
			Str("x_forwarded_for", r.Header.Get("X-Forwarded-For")).
			Str("x_real_ip", r.Header.Get("X-Real-IP")).
			Str("client_ip", resolveClientIP(r)).
			Msg("request complete")
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func corsMiddleware(cfg originConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			method := r.Method

			if method == http.MethodGet || method == http.MethodHead {
				if origin != "" && cfg.allowAnyGet {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
				}
			} else if method == http.MethodPost || method == http.MethodDelete || method == http.MethodOptions {
				allowed := originAllowed(origin, cfg.origins)
				if allowed {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
					w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
					w.Header().Set("Access-Control-Max-Age", "600")
				}

				if origin != "" && !allowed {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}

			if method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func uploadRateLimitMiddleware(limiter *ipRateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}

			if r.URL.Path == "/avatars" || r.URL.Path == "/exercise-images" {
				next.ServeHTTP(w, r)
				return
			}

			ip := resolveClientIP(r)
			allowed, count, windowFrom := limiter.AllowDetailed(ip)
			if !allowed {
				log.Warn().
					Str("ip", ip).
					Str("path", r.URL.Path).
					Int("count", count).
					Int("limit", limiter.limit).
					Dur("window", limiter.window).
					Time("window_from", windowFrom).
					Msg("rate limit exceeded")
				writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func resolveClientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if idx := strings.Index(xff, ","); idx >= 0 {
			xff = strings.TrimSpace(xff[:idx])
		}
		if xff != "" {
			return xff
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	return clientIP(r.RemoteAddr)
}

func newIPRateLimiter(limit int, window, ttl time.Duration) *ipRateLimiter {
	rl := &ipRateLimiter{
		limiters: map[string]*visitor{},
		limit:    limit,
		window:   window,
		ttl:      ttl,
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *ipRateLimiter) Allow(ip string) bool {
	allowed, _, _ := rl.AllowDetailed(ip)
	return allowed
}

func (rl *ipRateLimiter) AllowDetailed(ip string) (bool, int, time.Time) {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	v, exists := rl.limiters[ip]
	if !exists {
		v = &visitor{
			count:      0,
			windowFrom: now,
			lastAccess: now,
		}
		rl.limiters[ip] = v
	}

	if now.Sub(v.windowFrom) >= rl.window {
		v.windowFrom = now
		v.count = 0
	}

	v.lastAccess = now
	if v.count >= rl.limit {
		return false, v.count, v.windowFrom
	}
	v.count++
	return true, v.count, v.windowFrom
}

func (rl *ipRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-rl.ttl)
		rl.mu.Lock()
		for ip, v := range rl.limiters {
			if v.lastAccess.Before(cutoff) {
				delete(rl.limiters, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func parseOrigins(raw string) originConfig {
	cfg := originConfig{
		allowAnyGet: true,
		origins:     map[string]struct{}{},
	}
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		cfg.origins[value] = struct{}{}
	}
	return cfg
}

func originAllowed(origin string, allowed map[string]struct{}) bool {
	if origin == "" {
		return false
	}
	_, ok := allowed[origin]
	return ok
}

func resolveFullURL(baseURL string, r *http.Request, route string) string {
	if strings.TrimSpace(baseURL) != "" {
		return strings.TrimRight(baseURL, "/") + route
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xfProto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xfProto != "" {
		scheme = xfProto
	}
	return scheme + "://" + r.Host + route
}

func isAllowedType(file multipart.File, hdr *multipart.FileHeader, targetFilename string, cfg uploadConfig) bool {
	ctype := normalizeContentType(hdr.Header.Get("Content-Type"))
	uploadExt := strings.ToLower(filepath.Ext(hdr.Filename))
	targetExt := strings.ToLower(filepath.Ext(targetFilename))

	if uploadExt != "" {
		if _, ok := cfg.allowedExts[uploadExt]; !ok {
			return false
		}
	}
	if targetExt == "" {
		return false
	}
	if _, ok := cfg.allowedExts[targetExt]; !ok {
		return false
	}

	if ctype != "" {
		if _, ok := cfg.allowedMIMEs[ctype]; ok {
			return true
		}
	}

	head := make([]byte, 512)
	n, err := file.Read(head)
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false
	}
	detected := normalizeContentType(http.DetectContentType(head[:n]))
	if _, ok := cfg.allowedMIMEs[detected]; ok {
		return true
	}

	if targetExt == ".heic" || uploadExt == ".heic" {
		_, ok := cfg.allowedExts[".heic"]
		return ok
	}
	return false
}

func normalizeContentType(value string) string {
	if value == "" {
		return ""
	}
	parts := strings.Split(value, ";")
	return strings.ToLower(strings.TrimSpace(parts[0]))
}

func contentTypeForFilename(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ct := mime.TypeByExtension(ext); ct != "" {
		return strings.SplitN(ct, ";", 2)[0]
	}
	return "application/octet-stream"
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: message})
}

func calculateAudioDurationSeconds(path, filename string) (int64, error) {
	ext := strings.ToLower(filepath.Ext(filename))

	switch ext {
	case ".mp3":
		return calculateMP3Duration(path)
	case ".m4a", ".aac":
		return 0, nil
	default:
		return 0, fmt.Errorf("unsupported audio format: %s", ext)
	}
}

func calculateMP3Duration(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	decoder := mp3.NewDecoder(f)
	var frame mp3.Frame
	var skipped int
	var total time.Duration
	for {
		if err := decoder.Decode(&frame, &skipped); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, err
		}
		total += frame.Duration()
	}
	return int64(total.Seconds()), nil
}

func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func set(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func isValidFilename(name string) bool {
	if name == "" {
		return false
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	if name[0] == '.' {
		return false
	}
	cleaned := filepath.Clean(name)
	return cleaned == name
}

func registerMIMETypes() {
	types := map[string]string{
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".webp": "image/webp",
		".heic": "image/heic",
		".mp3":  "audio/mpeg",
		".m4a":  "audio/mp4",
		".aac":  "audio/aac",
	}
	for ext, ct := range types {
		_ = mime.AddExtensionType(ext, ct)
	}
}

// migrateVolumeToR2 walks the legacy Railway volume layout and uploads every
// file to R2 with key `<dir>/<filename>`. Idempotent: skips keys that already
// exist in R2.
func migrateVolumeToR2(ctx context.Context, r2c *r2Client, basePath string) map[string]any {
	result := map[string]any{
		"status":   "success",
		"uploaded": []string{},
		"skipped":  []string{},
		"failed":   []string{},
		"errors":   []string{},
	}

	// (sourceDir → R2 key prefix). The first two are the standard layout under
	// MEDIA_STORAGE_PATH; the last is the legacy meditation-audio direct volume
	// mount.
	dirMappings := []struct {
		src       string
		keyPrefix string
	}{
		{filepath.Join(basePath, "avatars"), "avatars"},
		{filepath.Join(basePath, "exercise-images"), "exercise-images"},
		{filepath.Join(basePath, "meditation-tracks"), "meditation-tracks"},
		{"/data/meditation-audio", "meditation-tracks"},
	}

	uploaded := []string{}
	skipped := []string{}
	failed := []string{}
	errs := []string{}

	for _, m := range dirMappings {
		entries, err := os.ReadDir(m.src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, fmt.Sprintf("read %s: %v", m.src, err))
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			filename := entry.Name()
			if !isValidFilename(filename) {
				continue
			}
			localPath := filepath.Join(m.src, filename)
			key := m.keyPrefix + "/" + filename

			if _, err := r2c.head(ctx, key); err == nil {
				skipped = append(skipped, key)
				continue
			} else if !isR2NotFound(err) {
				errs = append(errs, fmt.Sprintf("head %s: %v", key, err))
				failed = append(failed, key)
				continue
			}

			info, err := entry.Info()
			if err != nil {
				errs = append(errs, fmt.Sprintf("stat %s: %v", localPath, err))
				failed = append(failed, key)
				continue
			}

			f, err := os.Open(localPath)
			if err != nil {
				errs = append(errs, fmt.Sprintf("open %s: %v", localPath, err))
				failed = append(failed, key)
				continue
			}
			ct := contentTypeForFilename(filename)
			putErr := r2c.put(ctx, key, ct, f, info.Size())
			_ = f.Close()
			if putErr != nil {
				errs = append(errs, fmt.Sprintf("put %s: %v", key, putErr))
				failed = append(failed, key)
				log.Error().Err(putErr).Str("key", key).Msg("migration upload failed")
				continue
			}

			uploaded = append(uploaded, key)
			log.Info().Str("key", key).Int64("size", info.Size()).Msg("migrated to R2")
		}
	}

	result["uploaded"] = uploaded
	result["skipped"] = skipped
	result["failed"] = failed
	result["errors"] = errs
	if len(failed) > 0 {
		result["status"] = "partial"
	}
	return result
}

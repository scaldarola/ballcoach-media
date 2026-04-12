package main

import (
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
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/tcolgate/mp3"
)

const (
	maxImageSize = 10 * 1024 * 1024
	maxMP3Size   = 50 * 1024 * 1024
)

type uploadConfig struct {
	dirName       string
	maxSize       int64
	allowedMIMEs  map[string]struct{}
	allowedExts   map[string]struct{}
	responseRoute string
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

	storagePath := os.Getenv("MEDIA_STORAGE_PATH")
	if storagePath == "" {
		storagePath = "/data/media"
	}

	baseURL := os.Getenv("MEDIA_API_BASE_URL")
	registerMIMETypes()
	origins := parseOrigins(os.Getenv("CORS_ORIGINS"))
	limiter := newIPRateLimiter(20, 10*time.Second, 10*time.Minute)

	r := chi.NewRouter()
	r.Use(requestLogger)
	r.Use(corsMiddleware(origins))
	r.Use(uploadRateLimitMiddleware(limiter))

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		available, freeMB := storageHealth(storagePath)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":            "ok",
			"storage_available": available,
			"disk_space_mb":     freeMB,
		})
	})

	// TEMPORARY: List migrated files (will be removed after verification)
	r.Get("/admin/list-files", func(w http.ResponseWriter, r *http.Request) {
		result := map[string]any{
			"avatars":          []string{},
			"exercise_images":  []string{},
			"meditation_tracks": []string{},
		}

		// List avatars
		avatarDir := filepath.Join(storagePath, "avatars")
		if entries, err := os.ReadDir(avatarDir); err == nil {
			avatars := make([]string, 0)
			for _, entry := range entries {
				if !entry.IsDir() {
					avatars = append(avatars, entry.Name())
				}
			}
			result["avatars"] = avatars
		}

		// List exercise images
		exerciseDir := filepath.Join(storagePath, "exercise-images")
		if entries, err := os.ReadDir(exerciseDir); err == nil {
			exercises := make([]string, 0)
			for _, entry := range entries {
				if !entry.IsDir() {
					exercises = append(exercises, entry.Name())
				}
			}
			result["exercise_images"] = exercises
		}

		// List meditation tracks
		meditationDir := filepath.Join(storagePath, "meditation-tracks")
		if entries, err := os.ReadDir(meditationDir); err == nil {
			tracks := make([]string, 0)
			for _, entry := range entries {
				if !entry.IsDir() {
					tracks = append(tracks, entry.Name())
				}
			}
			result["meditation_tracks"] = tracks
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})

	r.Post("/avatars", uploadHandler(storagePath, baseURL, uploadConfig{
		dirName:       "avatars",
		maxSize:       maxImageSize,
		allowedMIMEs:  set("image/jpeg", "image/png", "image/webp", "image/heic"),
		allowedExts:   set(".jpg", ".jpeg", ".png", ".webp", ".heic"),
		responseRoute: "/avatars/%s",
	}, false))

	r.Post("/exercise-images", uploadHandler(storagePath, baseURL, uploadConfig{
		dirName:       "exercise-images",
		maxSize:       maxImageSize,
		allowedMIMEs:  set("image/jpeg", "image/png", "image/webp", "image/heic"),
		allowedExts:   set(".jpg", ".jpeg", ".png", ".webp", ".heic"),
		responseRoute: "/exercise-images/%s",
	}, false))

	r.Post("/meditation-tracks", uploadHandler(storagePath, baseURL, uploadConfig{
		dirName:       "meditation-tracks",
		maxSize:       maxMP3Size,
		allowedMIMEs:  set("audio/mpeg"),
		allowedExts:   set(".mp3"),
		responseRoute: "/meditation-tracks/%s/stream",
	}, true))

	r.Get("/avatars/{filename}", serveFileHandler(storagePath, "avatars", 24*time.Hour))
	r.Get("/exercise-images/{filename}", serveFileHandler(storagePath, "exercise-images", 24*time.Hour))
	r.Get("/meditation-tracks/{filename}/stream", serveFileHandler(storagePath, "meditation-tracks", 7*24*time.Hour))

	r.Delete("/avatars/{filename}", deleteFileHandler(storagePath, "avatars"))
	r.Delete("/exercise-images/{filename}", deleteFileHandler(storagePath, "exercise-images"))
	r.Delete("/meditation-tracks/{filename}", deleteFileHandler(storagePath, "meditation-tracks"))

	log.Info().Str("port", port).Str("storage_path", storagePath).Msg("ballcoach-media listening")
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatal().Err(err).Msg("server failed")
	}
}

func uploadHandler(storagePath, baseURL string, cfg uploadConfig, includeDuration bool) http.HandlerFunc {
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

		targetDir := filepath.Join(storagePath, cfg.dirName)
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			log.Error().Err(err).Str("dir", targetDir).Msg("failed to create directory")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		dstPath := filepath.Join(targetDir, filename)
		dst, err := os.Create(dstPath)
		if err != nil {
			log.Error().Err(err).Str("path", dstPath).Msg("failed to create destination file")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		written, copyErr := io.Copy(dst, io.LimitReader(file, cfg.maxSize+1))
		closeErr := dst.Close()
		if copyErr != nil || closeErr != nil {
			log.Error().Err(firstErr(copyErr, closeErr)).Str("path", dstPath).Msg("failed to save upload")
			_ = os.Remove(dstPath)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		if written > cfg.maxSize {
			_ = os.Remove(dstPath)
			writeJSONError(w, http.StatusBadRequest, "file too large")
			return
		}

		route := fmt.Sprintf(cfg.responseRoute, filename)
		resp := uploadResponse{
			URL:       route,
			FullURL:   resolveFullURL(baseURL, r, route),
			Filename:  filename,
			SizeBytes: written,
		}

		if includeDuration {
			durationSeconds, err := calculateMP3DurationSeconds(dstPath)
			if err != nil {
				log.Warn().Err(err).Str("path", dstPath).Msg("failed to calculate mp3 duration")
			}
			resp.DurationSeconds = durationSeconds
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func serveFileHandler(storagePath, dir string, cacheFor time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filename := chi.URLParam(r, "filename")
		if !isValidFilename(filename) {
			http.NotFound(w, r)
			return
		}

		path := filepath.Join(storagePath, dir, filename)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(cacheFor.Seconds())))
		if dir == "meditation-tracks" {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		http.ServeFile(w, r, path)
	}
}

func deleteFileHandler(storagePath, dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filename := chi.URLParam(r, "filename")
		if !isValidFilename(filename) {
			writeJSONError(w, http.StatusNotFound, "file not found")
			return
		}

		path := filepath.Join(storagePath, dir, filename)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			writeJSONError(w, http.StatusNotFound, "file not found")
			return
		}

		if err := os.Remove(path); err != nil {
			log.Error().Err(err).Str("path", path).Msg("failed to delete file")
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

				// Internal service-to-service requests may have no Origin header.
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

			if !limiter.Allow(clientIP(r.RemoteAddr)) {
				writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
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
		return false
	}
	v.count++
	return true
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

func storageHealth(path string) (bool, int64) {
	if err := os.MkdirAll(path, 0755); err != nil {
		log.Warn().Err(err).Str("path", path).Msg("storage path unavailable")
		return false, 0
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		log.Warn().Err(err).Str("path", path).Msg("storage stat failed")
		return false, 0
	}

	free := int64(stat.Bavail) * int64(stat.Bsize)
	return true, free / (1024 * 1024)
}

func isAllowedType(file multipart.File, hdr *multipart.FileHeader, targetFilename string, cfg uploadConfig) bool {
	ctype := normalizeContentType(hdr.Header.Get("Content-Type"))
	uploadExt := strings.ToLower(filepath.Ext(hdr.Filename))
	targetExt := strings.ToLower(filepath.Ext(targetFilename))

	// Validate both uploaded filename extension and target extension when present.
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

	// HEIC is often mislabeled; permit by extension if mime detection is ambiguous.
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

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: message})
}

func calculateMP3DurationSeconds(path string) (int64, error) {
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
	}
	for ext, ct := range types {
		mime.AddExtensionType(ext, ct)
	}
}

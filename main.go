package main

import (
	"encoding/json"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mediaDir := os.Getenv("MEDIA_DIR")
	if mediaDir == "" {
		mediaDir = "/data"
	}

	registerMIMETypes()

	r := chi.NewRouter()

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	r.Get("/media/{filename}", mediaHandler(mediaDir))

	log.Printf("ballcoach-media listening on :%s (serving from %s)", port, mediaDir)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatal(err)
	}
}

func mediaHandler(baseDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filename := chi.URLParam(r, "filename")

		if !isValidFilename(filename) {
			http.Error(w, "invalid filename", http.StatusBadRequest)
			return
		}

		path := filepath.Join(baseDir, filename)

		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")

		contentType := mime.TypeByExtension(filepath.Ext(filename))
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}

		http.ServeFile(w, r, path)
	}
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
		".mp3":  "audio/mpeg",
		".mp4":  "video/mp4",
		".aac":  "audio/aac",
		".m4a":  "audio/mp4",
	}
	for ext, ct := range types {
		mime.AddExtensionType(ext, ct)
	}
}

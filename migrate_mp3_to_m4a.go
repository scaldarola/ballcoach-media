// +build ignore

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Migration script to convert MP3 files to M4A format
// Usage: go run migrate_mp3_to_m4a.go [storage-path]
func main() {
	// Get storage path from args or use default
	storagePath := "/data/media"
	if len(os.Args) > 1 {
		storagePath = os.Args[1]
	}

	meditationDir := filepath.Join(storagePath, "meditation-tracks")

	fmt.Printf("🔍 Scanning for MP3 files in: %s\n", meditationDir)

	// Check if directory exists
	if _, err := os.Stat(meditationDir); os.IsNotExist(err) {
		fmt.Printf("❌ Directory does not exist: %s\n", meditationDir)
		os.Exit(1)
	}

	// Read directory
	entries, err := os.ReadDir(meditationDir)
	if err != nil {
		fmt.Printf("❌ Error reading directory: %v\n", err)
		os.Exit(1)
	}

	mp3Files := []string{}
	for _, entry := range entries {
		if !entry.IsDir() && strings.ToLower(filepath.Ext(entry.Name())) == ".mp3" {
			mp3Files = append(mp3Files, entry.Name())
		}
	}

	if len(mp3Files) == 0 {
		fmt.Println("✅ No MP3 files found. Migration not needed.")
		return
	}

	fmt.Printf("📝 Found %d MP3 file(s) to convert\n\n", len(mp3Files))

	// Check if ffmpeg is available
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		fmt.Println("❌ ffmpeg not found. Please install ffmpeg first.")
		os.Exit(1)
	}

	converted := 0
	failed := 0

	for i, filename := range mp3Files {
		fmt.Printf("[%d/%d] Converting: %s\n", i+1, len(mp3Files), filename)

		mp3Path := filepath.Join(meditationDir, filename)
		m4aFilename := strings.TrimSuffix(filename, ".mp3") + ".m4a"
		m4aPath := filepath.Join(meditationDir, m4aFilename)

		// Check if M4A already exists
		if _, err := os.Stat(m4aPath); err == nil {
			fmt.Printf("   ⚠️  M4A already exists: %s (skipping)\n\n", m4aFilename)
			continue
		}

		// Convert using ffmpeg
		// -i input.mp3: input file
		// -c:a aac: use AAC codec
		// -b:a 128k: 128kbps bitrate (good quality for meditation audio)
		// -vn: no video
		// -y: overwrite output file if exists
		cmd := exec.Command("ffmpeg",
			"-i", mp3Path,
			"-c:a", "aac",
			"-b:a", "128k",
			"-vn",
			"-y",
			m4aPath,
		)

		// Capture output
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("   ❌ Conversion failed: %v\n", err)
			fmt.Printf("   Output: %s\n\n", string(output))
			failed++
			continue
		}

		// Verify the M4A file was created
		if stat, err := os.Stat(m4aPath); err == nil {
			fmt.Printf("   ✅ Created: %s (%.2f MB)\n", m4aFilename, float64(stat.Size())/(1024*1024))
			converted++

			// Optional: Remove the original MP3 file
			// Uncomment the lines below if you want to delete MP3 files after conversion
			// if err := os.Remove(mp3Path); err != nil {
			// 	fmt.Printf("   ⚠️  Could not delete original MP3: %v\n", err)
			// } else {
			// 	fmt.Printf("   🗑️  Deleted original MP3\n")
			// }
		} else {
			fmt.Printf("   ❌ M4A file not created\n")
			failed++
		}

		fmt.Println()
	}

	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("✅ Migration complete!\n")
	fmt.Printf("   Converted: %d\n", converted)
	fmt.Printf("   Failed: %d\n", failed)
	fmt.Printf("   Total: %d\n", len(mp3Files))
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("\n📝 Note: Original MP3 files have been preserved.")
	fmt.Println("   You can delete them manually after verifying the M4A files work correctly.")
}

package services

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// archiveConvertedOriginal creates a preservation copy after successful HLS validation.
// Callers intentionally treat archive failures as non-fatal.
func archiveConvertedOriginal(source, archiveRoot, title, extension, fileID string) (string, error) {
	if strings.TrimSpace(archiveRoot) == "" { archiveRoot = "./videos/archives" }
	extension = strings.TrimPrefix(strings.ToLower(extension), ".")
	if actual := strings.TrimPrefix(strings.ToLower(filepath.Ext(source)), "."); actual == "mp4" || actual == "mkv" { extension = actual }
	if extension != "mp4" && extension != "mkv" { return "", fmt.Errorf("unsupported archive extension %q", extension) }

	safeTitle := sanitizeConvertedArchiveTitle(trimArchiveContainerMarker(title, extension))
	suffix := strings.ReplaceAll(strings.ToLower(fileID), "-", "")
	if suffix == "" { suffix = strings.ReplaceAll(uuid.NewString(), "-", "") }
	if len(suffix) > 8 { suffix = suffix[:8] }
	path := filepath.Join(archiveRoot, fmt.Sprintf("%s_%d_%s.%s", safeTitle, time.Now().UnixMilli(), suffix, extension))
	if err := os.MkdirAll(archiveRoot, 0755); err != nil { return "", fmt.Errorf("create archive directory %s: %w", archiveRoot, err) }
	if err := copyConvertedOriginal(source, path); err != nil { return "", fmt.Errorf("copy %s to archive %s: %w", source, path, err) }
	return path, nil
}

func trimArchiveContainerMarker(title, extension string) string {
	title = strings.TrimSpace(title)
	lowerTitle := strings.ToLower(title)
	marker := strings.ToLower(extension)
	for _, separator := range []string{"-", "_", ".", " "} {
		suffix := separator + marker
		if strings.HasSuffix(lowerTitle, suffix) && len(title) > len(suffix) {
			return strings.TrimSpace(title[:len(title)-len(suffix)])
		}
	}
	return title
}

func sanitizeConvertedArchiveTitle(title string) string {
	var b strings.Builder
	lastSeparator := false
	for _, r := range strings.TrimSpace(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			piece := string(r)
			if b.Len()+len(piece) > 160 { break }
			b.WriteString(piece)
			lastSeparator = false
		} else if (r == '-' || r == '_') && b.Len() > 0 {
			if !lastSeparator {
				if b.Len()+1 > 160 { break }
				b.WriteByte(r)
				lastSeparator = true
			}
		} else if b.Len() > 0 && !lastSeparator {
			if b.Len()+1 > 160 { break }
			b.WriteByte('-')
			lastSeparator = true
		}
	}
	name := strings.Trim(b.String(), "-_")
	if name == "" { return "video" }
	return name
}

func copyConvertedOriginal(source, destination string) error {
	in, err := os.Open(source)
	if err != nil { return err }
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil { return err }
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil { _ = os.Remove(destination); return copyErr }
	if closeErr != nil { _ = os.Remove(destination); return closeErr }
	return nil
}

package services

import (
	"ch/kirari04/videocms/models"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (w *WorkerGroup) EncoderCleanup(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		w.runEncoderCleanup()
		if !sleepContext(ctx, time.Minute) {
			return
		}
	}
}

/*
This function deletes the originally uploaded file after all qualitys and subtitles were encoded
*/
func (w *WorkerGroup) runEncoderCleanup() {
	// Archive cleanup is intentionally isolated from database/File.Path cleanup.
	// When enabled, it deletes only archived MP4/MKV files, never quality-folder sources.
	w.cleanupHLSConverterArchives()

	var dbReadyFiles []models.File
	if res := w.deps.DB.
		Preload("Qualitys").
		Preload("Subtitles").
		Preload("Audios").
		Not(&models.File{
			Path: "",
		}, "Path").
		Find(&dbReadyFiles); res.Error != nil {
		log.Printf("Failed to get PossibleDeleteTargets: %v", res.Error)
		return
	}

	var converterFileIDs []uint
	if err := w.deps.DB.Model(&models.ConverterLog{}).Distinct("file_id").Pluck("file_id", &converterFileIDs).Error; err != nil {
		log.Printf("Failed to load HLS-converter-managed files; skipping encoder cleanup to avoid deleting their originals: %v", err)
		return
	}
	converterManaged := make(map[uint]struct{}, len(converterFileIDs))
	for _, fileID := range converterFileIDs { converterManaged[fileID] = struct{}{} }

	for _, dbReadyFile := range dbReadyFiles {
		if _, managedByConverter := converterManaged[dbReadyFile.ID]; managedByConverter { continue }
		// Direct MP4/MKV qualities are the final media asset, not an encoded
		// derivative. Never delete their source path through encoder cleanup.
		hasDirectMedia := false
		for _, quality := range dbReadyFile.Qualitys {
			if strings.EqualFold(quality.Type, "mp4") || strings.EqualFold(quality.Type, "mkv") {
				hasDirectMedia = true
				break
			}
		}
		if hasDirectMedia {
			continue
		}

		var qualityAmount int64
		if res := w.deps.DB.
			Model(&models.Quality{}).
			Where(&models.Quality{
				FileID: dbReadyFile.ID,
				Ready:  true,
			}).
			Count(&qualityAmount); res.Error != nil {
			log.Printf("Failed to count quality by (delete candidate): Searcher ID %d inside database. Error: %v", dbReadyFile.ID, res.Error)
			continue
		}

		var subtitleAmount int64
		if res := w.deps.DB.
			Model(&models.Subtitle{}).
			Where(&models.Subtitle{
				FileID: dbReadyFile.ID,
				Ready:  true,
			}).
			Count(&subtitleAmount); res.Error != nil {
			log.Printf("Failed to count subtitle by (delete candidate): Searcher ID %d inside database. Error: %v", dbReadyFile.ID, res.Error)
			continue
		}

		var audioAmount int64
		if res := w.deps.DB.
			Model(&models.Audio{}).
			Where(&models.Audio{
				FileID: dbReadyFile.ID,
				Ready:  true,
			}).
			Count(&audioAmount); res.Error != nil {
			log.Printf("Failed to count audio by (delete candidate): Searcher ID %d inside database. Error: %v", dbReadyFile.ID, res.Error)
			continue
		}

		// in case all qualitys are encoded or failed the original file can be deleted
		if qualityAmount == int64(len(dbReadyFile.Qualitys)) &&
			subtitleAmount == int64(len(dbReadyFile.Subtitles)) &&
			audioAmount == int64(len(dbReadyFile.Audios)) {
			if err := os.Remove(dbReadyFile.Path); err != nil {
				log.Printf("Failed to delete file from path (%v): %v", dbReadyFile.Path, err)
				continue
			}

			// The publisher already calculated and saved the output size.
			// Do not recalculate here: the published folder may be on
			// remote storage and therefore unavailable on the local filesystem.
			dbReadyFile.Path = ""
			if err := w.deps.DB.Save(&dbReadyFile).Error; err != nil {
				log.Printf("Failed to save file after cleanup (file ID %d): %v", dbReadyFile.ID, err)
			}
		}
	}
}


// cleanupHLSConverterArchives purges archived originals when the existing
// HLSConverterDeleteOrginal setting is enabled (for example, after backups).
// It deliberately scans only the configured archive directory and never searches
// for or deletes original videos from the quality or upload directories.
func (w *WorkerGroup) cleanupHLSConverterArchives() {
	cfg := w.Config()
	if !cfg.HLSConverterDeleteOrginal {
		return
	}

	archiveRoot := strings.TrimSpace(cfg.FolderVideoArchives)
	if archiveRoot == "" {
		archiveRoot = "./videos/archives"
	}
	archiveAbs, err := filepath.Abs(archiveRoot)
	if err != nil {
		log.Printf("[EncoderCleanup] Cannot resolve archive directory %s: %v", archiveRoot, err)
		return
	}
	for _, mediaRoot := range []string{cfg.PublishedQualitysRoot(), cfg.FolderVideoQualitysPriv, cfg.FolderVideoUploadsPriv} {
		if strings.TrimSpace(mediaRoot) == "" {
			continue
		}
		mediaAbs, absErr := filepath.Abs(mediaRoot)
		if absErr == nil && pathsOverlap(archiveAbs, mediaAbs) {
			log.Printf("[EncoderCleanup] Refusing to purge archive directory %s because it overlaps media directory %s", archiveAbs, mediaAbs)
			return
		}
	}

	entries, err := os.ReadDir(archiveRoot)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		log.Printf("[EncoderCleanup] Cannot read archive directory %s: %v", archiveRoot, err)
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !entry.Type().IsRegular() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".mp4" && ext != ".mkv" {
			continue
		}
		path := filepath.Join(archiveRoot, entry.Name())
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[EncoderCleanup] Failed to remove archived original %s: %v", path, err)
		} else {
			log.Printf("[EncoderCleanup] Removed archived original after cleanup was enabled: %s", path)
		}
	}
}

func pathsOverlap(pathA, pathB string) bool {
	inside := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		if err != nil {
			return false
		}
		return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel))
	}
	return inside(pathA, pathB) || inside(pathB, pathA)
}

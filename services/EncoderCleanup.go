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
	// Keep converted source deletion centralized in this worker. The setting
	// controls only converter-input cleanup; normal uploaded-file cleanup below
	// retains its existing behavior.
	cfg := w.Config()
	if cfg.HLSConverterDeleteOrginal {
		w.cleanupConvertedOriginals(context.Background(), int(cfg.HLSConverterBatchSize))
	}

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

	for _, dbReadyFile := range dbReadyFiles {
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


// cleanupConvertedOriginals removes converter inputs only after a successful
// conversion produced a valid HLS playlist and all referenced segments exist.
// HLSConverter is responsible for conversion only and never deletes source files.
func (w *WorkerGroup) cleanupConvertedOriginals(ctx context.Context, limit int) {
	if limit < 1 {
		limit = 2
	}

	var logs []models.ConverterLog
	if err := w.deps.DB.
		Where("status = ? AND input_path <> '' AND output_path <> ''", "success").
		Order("created_at ASC").
		Limit(limit * 4).
		Find(&logs).Error; err != nil {
		log.Printf("[EncoderCleanup] Converter cleanup query failed: %v", err)
		return
	}

	removed := 0
	for _, row := range logs {
		if ctx.Err() != nil || removed >= limit {
			return
		}
		if _, err := os.Stat(row.InputPath); err != nil {
			if os.IsNotExist(err) {
				_ = w.deps.DB.Model(&row).Update("status", "cleaned_up").Error
			}
			continue
		}
		if ok, segmentsOK, err := validateHLS(row.OutputPath); err != nil || !ok || !segmentsOK {
			continue
		}

		master, err := os.ReadFile(filepath.Join(row.OutputPath, "master.m3u8"))
		if err != nil || !strings.Contains(string(master), "video.m3u8") {
			continue
		}
		if err := os.Remove(row.InputPath); err != nil {
			log.Printf("[EncoderCleanup] Failed to remove converted source %s: %v", row.InputPath, err)
			continue
		}
		if err := w.deps.DB.Model(&row).Update("status", "cleaned_up").Error; err != nil {
			log.Printf("[EncoderCleanup] Source removed but conversion log update failed for %s: %v", row.InputPath, err)
		}
		now := time.Now()
		if err := w.deps.DB.Create(&models.ConverterLog{
			FileID: row.FileID, QualityID: row.QualityID, InputPath: row.InputPath,
			OutputPath: row.OutputPath, CycleKey: row.CycleKey, Status: "cleanup_success",
			StartedAt: now, FinishedAt: &now,
		}).Error; err != nil {
			log.Printf("[EncoderCleanup] Source removed but audit log creation failed for %s: %v", row.InputPath, err)
		}
		removed++
	}
}

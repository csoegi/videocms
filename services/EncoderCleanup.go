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

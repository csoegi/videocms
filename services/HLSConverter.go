package services

import (
	"bufio"
	"ch/kirari04/videocms/models"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
)

func (w *WorkerGroup) HLSConverter(ctx context.Context) {
	if ctx == nil { ctx = context.Background() }
	for {
		cfg := w.Config()
		if cfg.HLSConverterIntervalMins <= 0 {
			if !sleepContext(ctx, time.Minute) { return }
			continue
		}
		if !sleepContext(ctx, time.Duration(cfg.HLSConverterIntervalMins)*time.Minute) { return }
		cfg = w.Config()
		if !w.normalEncodingQueueIdle() {
			log.Printf("[HLSConverter] Normal encoding queue busy; skipping this cycle")
			continue
		}
		if cfg.HLSConverterDeleteOrginal {
			w.cleanupConvertedOriginals(ctx, int(cfg.HLSConverterBatchSize))
			continue
		}
		w.convertLegacyBatch(ctx, int(cfg.HLSConverterBatchSize))
	}
}

func (w *WorkerGroup) normalEncodingQueueIdle() bool {
	for _, model := range []interface{}{&models.Quality{}, &models.Audio{}, &models.Subtitle{}} {
		var count int64
		if err := w.deps.DB.Model(model).Where("encoding = ? OR (ready = ? AND publishing = ? AND failed = ?)", true, false, false, false).Count(&count).Error; err != nil {
			log.Printf("[HLSConverter] Queue check failed; deferring: %v", err)
			return false
		}
		if count > 0 { return false }
	}
	return true
}

func (w *WorkerGroup) convertLegacyBatch(ctx context.Context, limit int) {
	if limit < 1 { limit = 2 }
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil || !w.normalEncodingQueueIdle() { return }
		q, file, input, cycle, err := w.claimLegacyQuality()
		if err != nil {
			if err != gorm.ErrRecordNotFound { log.Printf("[HLSConverter] Claim failed: %v", err) }
			return
		}
		if err := w.convertOne(ctx, q, file, input, cycle); err != nil {
			log.Printf("[HLSConverter] Quality %d failed: %v", q.ID, err)
		}
	}
}

func (w *WorkerGroup) claimLegacyQuality() (models.Quality, models.File, string, string, error) {
	var chosen models.Quality
	var chosenFile models.File
	var input, cycle string
	root := w.Config().PublishedQualitysRoot()
	err := w.deps.DB.Transaction(func(tx *gorm.DB) error {
		var candidates []models.Quality
		if err := tx.Preload("File").Joins("JOIN files ON files.id = qualities.file_id").
			Where("qualities.type = ? AND qualities.ready = ? AND qualities.failed = ? AND qualities.encoding = ? AND qualities.publishing = ?", "mp4", true, false, false, false).
			Order("files.created_at DESC, qualities.id DESC").Limit(200).Find(&candidates).Error; err != nil { return err }
		for _, q := range candidates {
			if q.File.ID == 0 { continue }
			path := resolveConverterInput(root, q.File, q)
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() { continue }
			hash := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d", q.ID, info.Size(), info.ModTime().UnixNano())))
			key := hex.EncodeToString(hash[:])
			var failures int64
			if err := tx.Model(&models.ConverterLog{}).Where("quality_id = ? AND cycle_key = ? AND status = ?", q.ID, key, "failed").Count(&failures).Error; err != nil { return err }
			if failures >= 2 { continue }
			result := tx.Model(&models.Quality{}).Where("id = ? AND type = ? AND ready = ? AND failed = ? AND encoding = ? AND publishing = ?", q.ID, "mp4", true, false, false, false).Update("encoding", true)
			if result.Error != nil { return result.Error }
			if result.RowsAffected == 0 { continue }
			chosen, chosenFile, input, cycle = q, q.File, path, key
			return nil
		}
		return gorm.ErrRecordNotFound
	})
	return chosen, chosenFile, input, cycle, err
}

func resolveConverterInput(root string, file models.File, q models.Quality) string {
	candidates := []string{}
	if filepath.IsAbs(q.Path) { candidates = append(candidates, q.Path) }
	candidates = append(candidates,
		filepath.Join(root, q.Path),
		filepath.Join(root, file.UUID, q.Path),
		filepath.Join(root, file.UUID, filepath.Base(q.Path)),
		filepath.Join(root, file.UUID, q.Name, "video.mp4"),
		filepath.Join(root, file.UUID, q.Name, "original.mp4"))
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() { return candidate }
	}
	if len(candidates) == 0 { return filepath.Join(root, file.UUID, filepath.Base(q.Path)) }
	return candidates[0]
}

func (w *WorkerGroup) convertOne(ctx context.Context, q models.Quality, file models.File, input, cycle string) error {
	started := time.Now()
	outDir := filepath.Dir(input)
	temp, err := os.MkdirTemp(outDir, ".hls-convert-")
	if err != nil { return w.logConverterFailure(q, file, input, outDir, cycle, 1, started, err) }
	defer os.RemoveAll(temp)
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		attemptStart := time.Now()
		row := models.ConverterLog{FileID:file.ID, QualityID:q.ID, InputPath:input, OutputPath:outDir, CycleKey:cycle, Attempt:attempt, Status:"processing", StartedAt:attemptStart}
		if err := w.deps.DB.Create(&row).Error; err != nil { lastErr = err; break }
		attemptDir := filepath.Join(temp, fmt.Sprintf("attempt-%d", attempt))
		if err := os.MkdirAll(attemptDir, 0755); err != nil { lastErr = err } else { lastErr = packageHLS(ctx, input, attemptDir) }
		playlistOK, segmentsOK := false, false
		if lastErr == nil { playlistOK, segmentsOK, lastErr = validateHLS(attemptDir) }
		if lastErr == nil {
			entries, e := os.ReadDir(attemptDir)
			if e != nil { lastErr = e } else {
				for _, entry := range entries {
					if e := os.Rename(filepath.Join(attemptDir, entry.Name()), filepath.Join(outDir, entry.Name())); e != nil { lastErr = e; break }
				}
			}
		}
		finished := time.Now()
		status := "failed"
		if lastErr == nil { status = "success" }
		w.deps.DB.Model(&row).Updates(map[string]interface{}{"status":status,"error":errText(lastErr),"playlist_valid":playlistOK,"segments_valid":segmentsOK,"finished_at":finished,"duration_ms":finished.Sub(attemptStart).Milliseconds()})
		if lastErr == nil {
			if err := w.deps.DB.Model(&models.Quality{}).Where("id = ? AND type = ? AND encoding = ?", q.ID, "mp4", true).
				Updates(map[string]interface{}{"type":"hls","output_file":"master.m3u8","path":converterStoredPath(w.Config().PublishedQualitysRoot(), outDir),"encoding":false,"progress":100,"ready":true,"failed":false,"error":""}).Error; err != nil { lastErr = err } else {
				return nil
			}
		}
		if attempt == 1 { log.Printf("[HLSConverter] Quality %d failed attempt 1; retrying once: %v", q.ID, lastErr) }
	}
	w.deps.DB.Model(&models.Quality{}).Where("id = ?", q.ID).Updates(map[string]interface{}{"encoding":false,"failed":true,"error":errText(lastErr)})
	return lastErr
}

func packageHLS(ctx context.Context, input, outDir string) error {
	probe := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "default=noprint_wrappers=1", input)
	output, err := probe.Output()
	if err != nil { return fmt.Errorf("ffprobe failed: %w", err) }
	hasVideo := false
	codecName := ""
	codecType := ""
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) != 2 { continue }
		switch parts[0] {
		case "codec_name": codecName = strings.ToLower(parts[1])
		case "codec_type":
			codecType = strings.ToLower(parts[1])
			if codecType == "video" {
				if codecName != "" && codecName != "h264" { return fmt.Errorf("unsupported video codec %q; stream copy only", codecName) }
				hasVideo = true
			}
			if codecType == "audio" && codecName != "" && codecName != "aac" && codecName != "mp3" && codecName != "ac3" {
				return fmt.Errorf("unsupported audio codec %q; stream copy only", codecName)
			}
		}
		if codecType == "video" && codecName != "" && codecName != "h264" { return fmt.Errorf("unsupported video codec %q; stream copy only", codecName) }
		if codecType == "audio" && codecName != "" && codecName != "aac" && codecName != "mp3" && codecName != "ac3" { return fmt.Errorf("unsupported audio codec %q; stream copy only", codecName) }
	}
	if !hasVideo { return fmt.Errorf("no H.264 video stream found") }
	media := filepath.Join(outDir, "video.m3u8")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-nostdin", "-y", "-v", "error", "-i", input,
		"-map", "0:v:0", "-map", "0:a:0?", "-c", "copy", "-f", "hls", "-hls_time", "4",
		"-hls_playlist_type", "vod", "-hls_segment_type", "mpegts", "-hls_list_size", "0",
		"-start_number", "1", "-hls_flags", "independent_segments", "-hls_segment_filename",
		filepath.Join(outDir, "out%d.ts"), media)
	if output, err := cmd.CombinedOutput(); err != nil { return fmt.Errorf("ffmpeg stream-copy failed: %w: %s", err, strings.TrimSpace(string(output))) }
	return os.WriteFile(filepath.Join(outDir, "master.m3u8"), []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=800000,AVERAGE-BANDWIDTH=600000\nvideo.m3u8\n"), 0644)
}

func validateHLS(dir string) (bool, bool, error) {
	path := filepath.Join(dir, "video.m3u8")
	f, err := os.Open(path)
	if err != nil { return false, false, err }
	defer f.Close()
	scanner := bufio.NewScanner(f)
	refs := []string{}
	end := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "#EXT-X-ENDLIST" { end = true }
		if line != "" && !strings.HasPrefix(line, "#") { refs = append(refs, line) }
	}
	if err := scanner.Err(); err != nil { return false, false, err }
	if !end || len(refs) == 0 { return false, false, fmt.Errorf("media playlist is incomplete or contains no segments") }
	for _, ref := range refs {
		if filepath.IsAbs(ref) || strings.Contains(ref, "..") { return true, false, fmt.Errorf("unsafe segment reference %q", ref) }
		info, err := os.Stat(filepath.Join(dir, ref))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 { return true, false, fmt.Errorf("missing or empty segment %q", ref) }
	}
	if _, err := os.Stat(filepath.Join(dir, "master.m3u8")); err != nil { return false, true, err }
	return true, true, nil
}

func (w *WorkerGroup) logConverterFailure(q models.Quality, file models.File, input, output, cycle string, attempt int, started time.Time, cause error) error {
	finished := time.Now()
	w.deps.DB.Create(&models.ConverterLog{FileID:file.ID,QualityID:q.ID,InputPath:input,OutputPath:output,CycleKey:cycle,Attempt:attempt,Status:"failed",Error:errText(cause),StartedAt:started,FinishedAt:&finished,DurationMs:finished.Sub(started).Milliseconds()})
	w.deps.DB.Model(&models.Quality{}).Where("id = ?",q.ID).Updates(map[string]interface{}{"encoding":false,"failed":true,"error":errText(cause)})
	return cause
}

func (w *WorkerGroup) cleanupConvertedOriginals(ctx context.Context, limit int) {
	if limit < 1 { limit = 2 }
	var logs []models.ConverterLog
	if err := w.deps.DB.Where("status = ? AND input_path <> ''", "success").Order("created_at DESC").Limit(limit * 4).Find(&logs).Error; err != nil {
		log.Printf("[HLSConverter] Cleanup log query failed: %v", err)
		return
	}
	removed := 0
	for _, row := range logs {
		if ctx.Err() != nil || removed >= limit { return }
		if _, err := os.Stat(row.InputPath); err != nil { continue }
		if row.OutputPath == "" { continue }
		if ok, _, err := validateHLS(row.OutputPath); err != nil || !ok { continue }
		master, err := os.ReadFile(filepath.Join(row.OutputPath, "master.m3u8"))
		if err != nil || !strings.Contains(string(master), "video.m3u8") { continue }
		if err := os.Remove(row.InputPath); err != nil {
			log.Printf("[HLSConverter] Cleanup failed for %s: %v", row.InputPath, err)
			continue
		}
		now := time.Now()
		if err := w.deps.DB.Create(&models.ConverterLog{FileID:row.FileID, QualityID:row.QualityID, InputPath:row.InputPath, OutputPath:row.OutputPath, CycleKey:row.CycleKey, Status:"cleanup_success", StartedAt:now, FinishedAt:&now}).Error; err != nil {
			log.Printf("[HLSConverter] Original deleted but cleanup audit log failed for %s: %v", row.InputPath, err)
		}
		removed++
	}
}

func converterStoredPath(root, outDir string) string {
	rel, err := filepath.Rel(root, filepath.Join(outDir, "master.m3u8"))
	if err != nil { return filepath.Join(filepath.Base(outDir), "master.m3u8") }
	return rel
}

func errText(err error) string { if err == nil { return "" }; return err.Error() }

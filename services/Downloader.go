package services

import (
	"ch/kirari04/videocms/models"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

func (w *WorkerGroup) Downloader(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !sleepContext(ctx, time.Second*5) {
		return
	}
	w.resetStaleRemoteDownloads()

	for {
		if w.remoteDownloadsEnabled() {
			w.loadDownloadTasks(ctx)
		}
		if !sleepContext(ctx, time.Second*5) {
			return
		}
	}
}

func (w *WorkerGroup) loadDownloadTasks(ctx context.Context) {
	availableSlots := w.availableRemoteDownloadSlots()
	if availableSlots <= 0 {
		return
	}

	var pendingDownloads []models.RemoteDownload
	w.deps.DB.
		Where("status = ?", models.RemoteDownloadStatusPending).
		Order("created_at ASC").
		Limit(availableSlots).
		Find(&pendingDownloads)

	for _, download := range pendingDownloads {
		if ctx.Err() != nil {
			return
		}
		if w.availableRemoteDownloadSlots() <= 0 {
			return
		}
		if !w.remoteDownloadsEnabled() {
			return
		}

		now := time.Now()
		claimed := w.deps.DB.Model(&models.RemoteDownload{}).
			Where("id = ? AND status = ?", download.ID, models.RemoteDownloadStatusPending).
			Updates(map[string]interface{}{
				"status":     models.RemoteDownloadStatusDownloading,
				"stage":      "Preparing source",
				"started_at": &now,
				"error":      "",
				"progress":   0,
			})
		if claimed.Error != nil || claimed.RowsAffected != 1 {
			continue
		}

		download.Status = models.RemoteDownloadStatusDownloading
		download.Stage = "Preparing source"
		download.StartedAt = &now
		downloadCtx, cancel := context.WithCancel(ctx)
		w.registerActiveDownload(download.ID, cancel)

		go func(task models.RemoteDownload) {
			defer w.unregisterActiveDownload(task.ID)
			w.processDownload(downloadCtx, task)
		}(download)
	}
}

func (w *WorkerGroup) availableRemoteDownloadSlots() int {
	maxDownloads := 1
	cfg := w.Config()
	if cfg.MaxParallelDownloads > 0 {
		maxDownloads = int(cfg.MaxParallelDownloads)
	}

	w.activeDownloadsMu.Lock()
	activeCount := len(w.activeDownloadCancels)
	w.activeDownloadsMu.Unlock()

	return maxDownloads - activeCount
}

func (w *WorkerGroup) remoteDownloadsEnabled() bool {
	cfg := w.Config()
	return cfg.RemoteDownloadEnabled == nil || *cfg.RemoteDownloadEnabled
}

func (w *WorkerGroup) registerActiveDownload(id uint, cancel context.CancelFunc) {
	w.activeDownloadsMu.Lock()
	w.activeDownloadCancels[id] = cancel
	w.activeDownloadsMu.Unlock()
}

func (w *WorkerGroup) unregisterActiveDownload(id uint) {
	w.activeDownloadsMu.Lock()
	delete(w.activeDownloadCancels, id)
	w.activeDownloadsMu.Unlock()
}

func (w *WorkerGroup) activeRemoteDownloadIDs() map[uint]bool {
	w.activeDownloadsMu.Lock()
	defer w.activeDownloadsMu.Unlock()

	ids := make(map[uint]bool, len(w.activeDownloadCancels))
	for id := range w.activeDownloadCancels {
		ids[id] = true
	}
	return ids
}

func (w *WorkerGroup) processDownload(parentCtx context.Context, task models.RemoteDownload) {
	if w.isCancelRequested(task.ID) {
		w.finishCanceled(&task, "Download canceled")
		return
	}
	if err := validateRemoteURLScheme(task.Url); err != nil {
		w.failDownload(&task, err.Error())
		return
	}
	w.setRemoteDownloadStage(&task, "Fetching source", 0)

	downloadCtx := parentCtx
	cancelTimeout := func() {}
	cfg := w.Config()
	if cfg.RemoteDownloadTimeout > 0 {
		downloadCtx, cancelTimeout = context.WithTimeout(parentCtx, time.Duration(cfg.RemoteDownloadTimeout)*time.Second)
	}
	defer cancelTimeout()

	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, task.Url, nil)
	if err != nil {
		w.failOrCancelDownload(&task, fmt.Sprintf("Failed to create request: %v", err))
		return
	}

	resp, err := secureHTTPClient().Do(req)
	if err != nil {
		w.failOrCancelDownload(&task, fmt.Sprintf("Network error: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		w.failDownload(&task, fmt.Sprintf("HTTP error: %s", resp.Status))
		return
	}

	isHLSInput := isHLSPlaylistResponse(task.Url, resp)
	if isHLSInput {
		w.setRemoteDownloadStage(&task, "Downloading HLS segments and remuxing to MP4", 0.10)
	} else {
		w.setRemoteDownloadStage(&task, "Downloading file", 0)
	}
	hlsSourceURL := task.Url
	if resp.Request != nil && resp.Request.URL != nil {
		hlsSourceURL = resp.Request.URL.String()
	}
	totalSize := int64(0)
	if resp.ContentLength > 0 {
		totalSize = resp.ContentLength
	}
	cfg = w.Config()
	// An HLS playlist's Content-Length describes only the small playlist, not
	// the final media. Its remux output is size-limited separately.
	if !isHLSInput && totalSize > 0 && cfg.MaxUploadFilesize > 0 && totalSize > cfg.MaxUploadFilesize {
		w.failDownload(&task, fmt.Sprintf("Remote file exceeds max upload size (%d bytes)", cfg.MaxUploadFilesize))
		return
	}

	var initialPlaylist []byte
	initialPlaylistContentType := resp.Header.Get("Content-Type")
	if isHLSInput {
		// Reuse the playlist body already fetched for type detection. Signed URLs
		// may be single-use or expire quickly, so asking FFmpeg to fetch the root
		// playlist again can make an otherwise valid job fail.
		const maxHLSPlaylistBytes = 4 << 20
		initialPlaylist, err = io.ReadAll(io.LimitReader(resp.Body, maxHLSPlaylistBytes+1))
		if err != nil {
			w.failDownload(&task, fmt.Sprintf("Failed to read HLS playlist: %v", err))
			return
		}
		if len(initialPlaylist) > maxHLSPlaylistBytes {
			w.failDownload(&task, "HLS playlist exceeds maximum allowed size")
			return
		}
	}

	fileName := resolveRemoteFileName(task, resp)
	if isHLSInput {
		fileName = hlsMP4OutputName(fileName, task.ID)
		totalSize = 0 // Output size is not known until FFmpeg finishes.
	}
	tempPath := filepath.Join(cfg.FolderVideoUploadsPriv, fmt.Sprintf("remote_%d_%s.download", task.ID, uuid.NewString()))
	if err := os.MkdirAll(filepath.Dir(tempPath), 0755); err != nil {
		w.failDownload(&task, fmt.Sprintf("Failed to prepare upload folder: %v", err))
		return
	}

	task.Name = fileName
	task.TotalSize = totalSize
	task.TempPath = tempPath
	initialProgress := 0.0
	if isHLSInput {
		initialProgress = 0.10
	}
	if res := w.deps.DB.Model(&task).Updates(map[string]interface{}{
		"name":       task.Name,
		"total_size": task.TotalSize,
		"temp_path":  task.TempPath,
		"progress":   initialProgress,
	}); res.Error != nil {
		w.failDownload(&task, "Failed to persist download metadata")
		return
	}

	var copied int64
	if isHLSInput {
		// The first safe HTTP request above verifies URL accessibility and helps
		// identify playlist responses. FFmpeg fetches the playlist and every
		// segment through a local proxy that enforces public-IP checks.
		_ = resp.Body.Close()
		if err := remuxRemoteHLS(downloadCtx, hlsSourceURL, initialPlaylist, initialPlaylistContentType, tempPath, cfg.MaxUploadFilesize); err != nil {
			cleanupRemoteTemp(tempPath, fileName)
			w.failOrCancelDownload(&task, fmt.Sprintf("HLS to MP4 conversion failed: %v", err))
			return
		}
		w.setRemoteDownloadStage(&task, "Validating MP4 output", 0.78)
		info, err := os.Stat(tempPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
			cleanupRemoteTemp(tempPath, fileName)
			w.failDownload(&task, "HLS conversion did not produce a valid MP4 file")
			return
		}
		copied = info.Size()
	} else {
		out, err := os.Create(tempPath)
		if err != nil {
			w.failDownload(&task, fmt.Sprintf("Failed to create temp file: %v", err))
			return
		}
		var copyErr error
		copied, copyErr = w.copyRemoteBody(downloadCtx, resp.Body, out, &task)
		closeErr := out.Close()
		if copyErr != nil {
			cleanupRemoteTemp(tempPath, fileName)
			w.failOrCancelDownload(&task, fmt.Sprintf("Download failed: %v", copyErr))
			return
		}
		if closeErr != nil {
			cleanupRemoteTemp(tempPath, fileName)
			w.failDownload(&task, fmt.Sprintf("Failed to close temp file: %v", closeErr))
			return
		}
	}

	if !isHLSInput {
		w.setRemoteDownloadStage(&task, "Validating downloaded file", 0.78)
	}
	task.BytesDownloaded = copied
	if w.isCancelRequested(task.ID) {
		cleanupRemoteTemp(tempPath, fileName)
		w.finishCanceled(&task, "Download canceled")
		return
	}

	if res := w.deps.DB.Model(&task).Updates(map[string]interface{}{
		"status":           models.RemoteDownloadStatusImporting,
		"stage":            "Registering media",
		"bytes_downloaded": task.BytesDownloaded,
		"progress":         0.88,
	}); res.Error != nil {
		cleanupRemoteTemp(tempPath, fileName)
		w.failDownload(&task, "Failed to update import status")
		return
	}
	task.Status = models.RemoteDownloadStatusImporting
	task.Stage = "Registering media"
	task.Progress = 0.88

	fileUUID := uuid.NewString()

	var status int
	var dbLink *models.Link
	var cloned bool
	encodingEnabled := w.Config().EncodingEnabled != nil && *w.Config().EncodingEnabled
	directMedia := isHLSInput || !encodingEnabled
	if isHLSInput {
		// HLS input is already encoded media. Publish the stream-copy MP4 directly
		// even when normal MP4 uploads are configured to go through HLS encoding.
		status, dbLink, cloned, err = w.logic.CreateFileDirect(
			&tempPath, task.ParentFolderID, task.Name, fileUUID,
			task.BytesDownloaded, task.UserID, "", "none", nil,
		)
	} else {
		status, dbLink, cloned, err = w.logic.CreateFile(
			&tempPath, task.ParentFolderID, task.Name, fileUUID,
			task.BytesDownloaded, task.UserID, "", "none", nil,
		)
	}

	if err != nil {
		cleanupRemoteTemp(tempPath, fileName)
		w.failDownload(&task, fmt.Sprintf("Import failed (status %d): %v", status, err))
		return
	}
	if dbLink == nil {
		cleanupRemoteTemp(tempPath, fileName)
		w.failDownload(&task, "Import failed: missing created link")
		return
	}

	if w.isCancelRequested(task.ID) {
		_ = w.deleteImportedRemoteLink(task.UserID, dbLink.ID)
		if cloned {
			cleanupRemoteTemp(tempPath, fileName)
		}
		w.finishCanceled(&task, "Download canceled")
		return
	}

	// For the direct-media path, the remote-download job stays active until the
	// publisher confirms the file is ready. Normal encoded downloads finish once
	// their source has been registered and queued for encoding.
	if directMedia && !cloned {
		w.setRemoteDownloadStage(&task, "Publishing media", 0.95)
		if err := w.waitForDirectMediaPublication(downloadCtx, task.ID, dbLink.FileID); err != nil {
			if w.isCancelRequested(task.ID) || errors.Is(err, context.Canceled) {
				_ = w.deleteImportedRemoteLink(task.UserID, dbLink.ID)
				w.finishCanceled(&task, "Download canceled")
				return
			}
			w.failDownload(&task, fmt.Sprintf("Media publication failed: %v", err))
			return
		}
	}

	if cloned {
		cleanupRemoteTemp(tempPath, fileName)
	}

	finishTime := time.Now()
	duration := 0.0
	if task.StartedAt != nil {
		duration = finishTime.Sub(*task.StartedAt).Seconds()
	}

	task.Status = models.RemoteDownloadStatusCompleted
	task.Stage = "Completed"
	task.FinishedAt = &finishTime
	task.Progress = 1.0
	task.Duration = duration
	task.LinkID = dbLink.ID
	task.LinkUUID = dbLink.UUID
	task.FileID = dbLink.FileID
	task.Error = ""
	task.TempPath = ""
	w.deps.DB.Save(&task)

	w.logStats(task)
}

// setRemoteDownloadStage persists a human-readable phase independently from
// byte-based progress. For HLS jobs, percentages are milestones because FFmpeg
// downloads and remuxes segments concurrently and does not know the final byte
// size in advance.
func (w *WorkerGroup) setRemoteDownloadStage(task *models.RemoteDownload, stage string, progress float64) {
	if task == nil {
		return
	}
	task.Stage = stage
	task.Progress = progress
	if err := w.deps.DB.Model(&models.RemoteDownload{}).Where("id = ?", task.ID).Updates(map[string]interface{}{
		"stage":    stage,
		"progress": progress,
	}).Error; err != nil {
		log.Printf("[Services->Downloader] Failed to persist stage %q for remote download %d: %v", stage, task.ID, err)
	}
}

// waitForDirectMediaPublication waits for the direct-media quality to transition
// to Ready. This keeps HLS/direct-MP4 downloads from appearing completed while
// their staging directory is still waiting for the publisher.
func (w *WorkerGroup) waitForDirectMediaPublication(ctx context.Context, downloadID uint, fileID uint) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if w.isCancelRequested(downloadID) {
			return context.Canceled
		}
		var qualities []models.Quality
		if err := w.deps.DB.Where("file_id = ? AND type IN ?", fileID, []string{"mp4", "mkv"}).Find(&qualities).Error; err != nil {
			return fmt.Errorf("query direct media quality: %w", err)
		}
		if len(qualities) > 0 {
			for _, quality := range qualities {
				if quality.Ready {
					return nil
				}
				if quality.Failed {
					if quality.Error != "" {
						return errors.New(quality.Error)
					}
					return errors.New("publisher marked the direct quality as failed")
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func isHLSPlaylistResponse(rawURL string, resp *http.Response) bool {
	if parsed, err := url.Parse(rawURL); err == nil && strings.EqualFold(filepath.Ext(parsed.Path), ".m3u8") {
		return true
	}
	if resp == nil {
		return false
	}
	if resp.Request != nil && resp.Request.URL != nil && strings.EqualFold(filepath.Ext(resp.Request.URL.Path), ".m3u8") {
		return true
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	return strings.Contains(contentType, "mpegurl") || strings.Contains(contentType, "m3u8")
}

func hlsMP4OutputName(inputName string, downloadID uint) string {
	base := strings.TrimSpace(strings.TrimSuffix(filepath.Base(inputName), filepath.Ext(inputName)))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = fmt.Sprintf("remote-download-%d", downloadID)
	}
	return sanitizeRemoteFileName(base+".mp4", downloadID)
}

func secureHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return dialPublicAddress(ctx, network, address)
		},
	}

	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return validateRemoteURLScheme(req.URL.String())
		},
	}
}

func dialPublicAddress(ctx context.Context, network string, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IP addresses found for %s", host)
	}

	for _, ip := range ips {
		if isBlockedRemoteIP(ip) {
			return nil, fmt.Errorf("remote host resolves to blocked IP %s", ip.String())
		}
	}

	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

func isBlockedRemoteIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsUnspecified() ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast()
}

func validateRemoteURLScheme(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("remote downloads only support http and https URLs")
	}
	if parsed.Host == "" {
		return errors.New("remote download URL is missing a host")
	}
	return nil
}

func resolveRemoteFileName(task models.RemoteDownload, resp *http.Response) string {
	if disposition := resp.Header.Get("Content-Disposition"); disposition != "" {
		_, params, err := mime.ParseMediaType(disposition)
		if err == nil {
			if filename := params["filename"]; filename != "" {
				return sanitizeRemoteFileName(filename, task.ID)
			}
			if filename := params["filename*"]; filename != "" {
				return sanitizeRemoteFileName(filename, task.ID)
			}
		}
	}

	if resp.Request != nil && resp.Request.URL != nil {
		if baseName := filepath.Base(resp.Request.URL.Path); baseName != "." && baseName != "/" && baseName != "" {
			if decoded, err := url.PathUnescape(baseName); err == nil {
				return sanitizeRemoteFileName(decoded, task.ID)
			}
			return sanitizeRemoteFileName(baseName, task.ID)
		}
	}

	return sanitizeRemoteFileName(fmt.Sprintf("remote-download-%d", task.ID), task.ID)
}

func sanitizeRemoteFileName(name string, id uint) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " ._")
	if name == "" {
		name = fmt.Sprintf("remote-download-%d", id)
	}

	const maxNameLength = 128
	runes := []rune(name)
	if len(runes) <= maxNameLength {
		return name
	}

	ext := filepath.Ext(name)
	extRunes := []rune(ext)
	if ext != "" && len(extRunes) < maxNameLength-8 {
		baseRunes := []rune(strings.TrimSuffix(name, ext))
		allowedBaseLength := maxNameLength - len(extRunes)
		if len(baseRunes) > allowedBaseLength {
			baseRunes = baseRunes[:allowedBaseLength]
		}
		return strings.TrimRight(string(baseRunes), " ._") + ext
	}
	return string(runes[:maxNameLength])
}

func (w *WorkerGroup) copyRemoteBody(ctx context.Context, src io.Reader, dst io.Writer, task *models.RemoteDownload) (int64, error) {
	buffer := make([]byte, 64*1024)
	var downloaded int64
	lastUpdate := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			return downloaded, err
		}

		n, readErr := src.Read(buffer)
		if n > 0 {
			downloaded += int64(n)
			cfg := w.Config()
			if cfg.MaxUploadFilesize > 0 && downloaded > cfg.MaxUploadFilesize {
				return downloaded, fmt.Errorf("remote file exceeds max upload size (%d bytes)", cfg.MaxUploadFilesize)
			}
			if _, err := dst.Write(buffer[:n]); err != nil {
				return downloaded, err
			}

			if time.Since(lastUpdate) > time.Second {
				task.BytesDownloaded = downloaded
				if task.TotalSize > 0 {
					task.Progress = float64(downloaded) / float64(task.TotalSize)
				}
				w.deps.DB.Model(task).Updates(map[string]interface{}{
					"bytes_downloaded": task.BytesDownloaded,
					"progress":         task.Progress,
				})
				lastUpdate = time.Now()
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				task.BytesDownloaded = downloaded
				if task.TotalSize > 0 {
					task.Progress = float64(downloaded) / float64(task.TotalSize)
				}
				w.deps.DB.Model(task).Updates(map[string]interface{}{
					"bytes_downloaded": task.BytesDownloaded,
					"progress":         task.Progress,
				})
				return downloaded, nil
			}
			return downloaded, readErr
		}
	}
}

func (w *WorkerGroup) failOrCancelDownload(task *models.RemoteDownload, reason string) {
	if w.isCancelRequested(task.ID) {
		cleanupRemoteTemp(task.TempPath, task.Name)
		w.finishCanceled(task, "Download canceled")
		return
	}
	w.failDownload(task, reason)
}

func (w *WorkerGroup) failDownload(task *models.RemoteDownload, reason string) {
	now := time.Now()
	task.Status = models.RemoteDownloadStatusFailed
	task.Stage = "Failed"
	task.Error = truncateRemoteError(reason)
	task.FinishedAt = &now
	if task.StartedAt != nil {
		task.Duration = now.Sub(*task.StartedAt).Seconds()
	}
	task.TempPath = ""
	w.deps.DB.Save(task)
}

func (w *WorkerGroup) finishCanceled(task *models.RemoteDownload, reason string) {
	now := time.Now()
	task.Status = models.RemoteDownloadStatusCanceled
	task.Stage = "Canceled"
	task.Error = truncateRemoteError(reason)
	task.FinishedAt = &now
	task.CanceledAt = &now
	task.Progress = 0
	if task.StartedAt != nil {
		task.Duration = now.Sub(*task.StartedAt).Seconds()
	}
	task.TempPath = ""
	w.deps.DB.Save(task)
}

func (w *WorkerGroup) isCancelRequested(id uint) bool {
	var current models.RemoteDownload
	if res := w.deps.DB.Select("status").First(&current, id); res.Error != nil {
		return false
	}
	return current.Status == models.RemoteDownloadStatusCanceling || current.Status == models.RemoteDownloadStatusCanceled
}

func cleanupRemoteTemp(tempPath string, fileName string) {
	if tempPath == "" {
		return
	}
	_ = os.Remove(tempPath)
	if ext := filepath.Ext(fileName); ext != "" {
		_ = os.Remove(tempPath + ext)
	}
}

func (w *WorkerGroup) deleteImportedRemoteLink(userID uint, linkID uint) error {
	if linkID == 0 {
		return nil
	}
	_, err := w.logic.DeleteFiles(&models.LinksDeleteValidation{
		LinkIDs: []models.LinkDeleteValidation{{LinkID: linkID}},
	}, userID, false)
	return err
}

func truncateRemoteError(reason string) string {
	const maxErrorLength = 1024
	if len(reason) <= maxErrorLength {
		return reason
	}
	return reason[:maxErrorLength]
}

func (w *WorkerGroup) CancelRemoteDownload(userID uint, downloadID uint) (int, error) {
	var task models.RemoteDownload
	if res := w.deps.DB.Where("id = ? AND user_id = ?", downloadID, userID).First(&task); res.Error != nil {
		return http.StatusNotFound, errors.New("download not found")
	}

	if models.IsRemoteDownloadTerminal(task.Status) {
		return http.StatusConflict, errors.New("download is already finished")
	}

	now := time.Now()
	switch task.Status {
	case models.RemoteDownloadStatusPending:
		task.Status = models.RemoteDownloadStatusCanceled
		task.Stage = "Canceled"
		task.Error = "Download canceled"
		task.CancelRequestedAt = &now
		task.CanceledAt = &now
		task.FinishedAt = &now
		if res := w.deps.DB.Save(&task); res.Error != nil {
			return http.StatusInternalServerError, errors.New("failed to cancel download")
		}
		return http.StatusOK, nil
	default:
		if res := w.deps.DB.Model(&task).Updates(map[string]interface{}{
			"status":              models.RemoteDownloadStatusCanceling,
			"error":               "Cancellation requested",
			"cancel_requested_at": &now,
		}); res.Error != nil {
			return http.StatusInternalServerError, errors.New("failed to request cancellation")
		}

		w.activeDownloadsMu.Lock()
		cancel := w.activeDownloadCancels[task.ID]
		w.activeDownloadsMu.Unlock()
		if cancel != nil {
			cancel()
			return http.StatusOK, nil
		}

		cleanupRemoteTemp(task.TempPath, task.Name)
		task.Status = models.RemoteDownloadStatusCanceled
		task.Stage = "Canceled"
		task.Error = "Download canceled"
		task.CanceledAt = &now
		task.FinishedAt = &now
		task.TempPath = ""
		w.deps.DB.Save(&task)
		return http.StatusOK, nil
	}
}

func (w *WorkerGroup) CancelAllRemoteDownloads(reason string) {
	now := time.Now()
	activeIDsMap := w.activeRemoteDownloadIDs()
	activeIDs := make([]uint, 0, len(activeIDsMap))
	for id := range activeIDsMap {
		activeIDs = append(activeIDs, id)
	}

	if len(activeIDs) > 0 {
		w.deps.DB.Model(&models.RemoteDownload{}).
			Where("id IN ? AND status IN ?", activeIDs, []string{
				models.RemoteDownloadStatusDownloading,
				models.RemoteDownloadStatusImporting,
				models.RemoteDownloadStatusCanceling,
			}).
			Updates(map[string]interface{}{
				"status":              models.RemoteDownloadStatusCanceling,
				"error":               truncateRemoteError(reason),
				"cancel_requested_at": &now,
			})

		w.activeDownloadsMu.Lock()
		for _, cancel := range w.activeDownloadCancels {
			cancel()
		}
		w.activeDownloadsMu.Unlock()
	}

	cancelQuery := w.deps.DB.Model(&models.RemoteDownload{}).
		Where("status = ?", models.RemoteDownloadStatusPending)
	if len(activeIDs) > 0 {
		cancelQuery = cancelQuery.Or("status IN ? AND id NOT IN ?", []string{
			models.RemoteDownloadStatusDownloading,
			models.RemoteDownloadStatusImporting,
			models.RemoteDownloadStatusCanceling,
		}, activeIDs)
	} else {
		cancelQuery = cancelQuery.Or("status IN ?", []string{
			models.RemoteDownloadStatusDownloading,
			models.RemoteDownloadStatusImporting,
			models.RemoteDownloadStatusCanceling,
		})
	}
	cancelQuery.Updates(map[string]interface{}{
		"status":              models.RemoteDownloadStatusCanceled,
		"error":               truncateRemoteError(reason),
		"cancel_requested_at": &now,
		"canceled_at":         &now,
		"finished_at":         &now,
		"temp_path":           "",
	})
}

func (w *WorkerGroup) resetStaleRemoteDownloads() {
	var staleDownloads []models.RemoteDownload
	if res := w.deps.DB.Where("status IN ?", []string{
		models.RemoteDownloadStatusDownloading,
		models.RemoteDownloadStatusImporting,
		models.RemoteDownloadStatusCanceling,
	}).Find(&staleDownloads); res.Error != nil {
		log.Printf("Failed to load stale remote downloads: %v", res.Error)
		return
	}

	now := time.Now()
	for _, task := range staleDownloads {
		cleanupRemoteTemp(task.TempPath, task.Name)
		if task.Status == models.RemoteDownloadStatusCanceling {
			task.Status = models.RemoteDownloadStatusCanceled
			task.Stage = "Canceled"
			task.Error = "Download canceled during server restart"
			task.CanceledAt = &now
		} else {
			task.Status = models.RemoteDownloadStatusFailed
			task.Stage = "Failed"
			task.Error = "Download interrupted by server restart"
		}
		task.FinishedAt = &now
		task.TempPath = ""
		w.deps.DB.Save(&task)
	}
}

func (w *WorkerGroup) logStats(task models.RemoteDownload) {
	domain := "unknown"
	u, err := url.Parse(task.Url)
	if err == nil && u.Host != "" {
		domain = u.Host
	} else {
		parts := strings.Split(task.Url, "//")
		if len(parts) > 1 {
			domain = strings.Split(parts[1], "/")[0]
		}
	}

	stat := models.RemoteDownloadLog{
		UserID:  task.UserID,
		Domain:  domain,
		Bytes:   uint64(task.BytesDownloaded),
		Seconds: task.Duration,
	}
	w.deps.DB.Create(&stat)
}

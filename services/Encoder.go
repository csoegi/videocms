package services

import (
	"ch/kirari04/videocms/models"
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/imroc/req/v3"
	"gorm.io/gorm"
)

type ActiveEncoding struct {
	Type    string
	FileID  uint
	ID      uint       // qualityID | audioID | subID
	Channel *chan bool // sending true will kill the encoding process
}

type EncodingTask struct {
	Type   string
	FileID uint
	ID     uint // qualityID | audioID | subID
}

type IwithProcess interface {
	SetProcess(float64)
	GetProcess() float64
	Save(DB *gorm.DB) *gorm.DB
}

func (w *WorkerGroup) Encoder(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	w.limitChan = make(chan bool, w.Config().MaxRunningEncodes)
	for {
		go w.loadEncodingTasks(ctx)
		if !sleepContext(ctx, time.Second*10) {
			return
		}
	}
}

func (w *WorkerGroup) ResetEncodingState() {
	if res := w.deps.DB.
		Model(&models.Quality{}).
		Where(&models.Quality{
			Encoding: true,
		}, "Encoding").
		Or("progress > ?", 0).
		Updates(map[string]interface{}{"encoding": false, "progress": 0}); res.Error != nil {
		log.Println("[Services->Encoder->ResetEncodingState] [ERROR]: Failed to reset encoding status on Quality", res.Error)
	}

	if res := w.deps.DB.
		Model(&models.Audio{}).
		Where(&models.Audio{
			Encoding: true,
		}, "Encoding").
		Or("progress > ?", 0).
		Updates(map[string]interface{}{"encoding": false, "progress": 0}); res.Error != nil {
		log.Println("[Services->Encoder->ResetEncodingState] [ERROR]: Failed to reset encoding status on Audio", res.Error)
	}

	if res := w.deps.DB.
		Model(&models.Subtitle{}).
		Where(&models.Subtitle{
			Encoding: true,
		}, "Encoding").
		Or("progress > ?", 0).
		Updates(map[string]interface{}{"encoding": false, "progress": 0}); res.Error != nil {
		log.Println("[Services->Encoder->ResetEncodingState] [ERROR]: Failed to reset encoding status on Subtitle", res.Error)
	}
}

func (w *WorkerGroup) loadEncodingTasks(ctx context.Context) {
	var encodingTasks []EncodingTask

	// SUBTITLES
	// we want to encode the subtitles first, then audio and in the end the qualities
	var encodingSubs []models.Subtitle
	if err := w.deps.DB.
		Model(&models.Subtitle{}).
		Preload("File").
		Where(&models.Subtitle{
			Encoding:   false,
			Publishing: false,
			Ready:      false,
			Failed:     false,
		}, "Encoding", "Publishing", "Ready", "Failed").
		// 🌟 Exclude external subtitles from encoding
		Not("source = ?", "external").
		Order("id ASC").
		Limit(10).
		Find(&encodingSubs).Error; err != nil {
		log.Printf("[Services->Encoder->loadEncodingTasks] [ERROR]: Failed to query queued subtitles: %v", err)
		return
	}

	if len(encodingSubs) > 0 {
		log.Printf("[Services->Encoder->loadEncodingTasks] [INFO]: Loaded %v subs to encode", len(encodingSubs))
	}

	for _, v := range encodingSubs {
		v.Encoding = true
		if err := v.Save(w.deps.DB).Error; err != nil {
			log.Printf("[Services->Encoder->loadEncodingTasks] [ERROR]: Failed to claim subtitle %d: %v", v.ID, err)
			continue
		}
		encodingTasks = append(encodingTasks, EncodingTask{
			Type:   "sub",
			FileID: v.FileID,
			ID:     v.ID,
		})
	}

	// AUDIOS
	var encodingAudios []models.Audio
	if len(encodingSubs) < 10 {
		if err := w.deps.DB.
			Model(&models.Audio{}).
			Preload("File").
			Where(&models.Audio{
				Encoding:   false,
				Publishing: false,
				Ready:      false,
				Failed:     false,
			}, "Encoding", "Publishing", "Ready", "Failed").
			Order("id ASC").
			Limit(10).
			Find(&encodingAudios).Error; err != nil {
			log.Printf("[Services->Encoder->loadEncodingTasks] [ERROR]: Failed to query queued audio tracks: %v", err)
			return
		}
	}

	if len(encodingAudios) > 0 {
		log.Printf("[Services->Encoder->loadEncodingTasks] [INFO]: Loaded %v audios to encode", len(encodingAudios))
	}

	for _, v := range encodingAudios {
		v.Encoding = true
		if err := v.Save(w.deps.DB).Error; err != nil {
			log.Printf("[Services->Encoder->loadEncodingTasks] [ERROR]: Failed to claim audio %d: %v", v.ID, err)
			continue
		}
		encodingTasks = append(encodingTasks, EncodingTask{
			Type:   "audio",
			FileID: v.FileID,
			ID:     v.ID,
		})
	}

	// QUALITYS
	var encodingQualitys []models.Quality
	if len(encodingSubs) < 10 && len(encodingAudios) < 10 {
		if err := w.deps.DB.
			Model(&models.Quality{}).
			Preload("File").
			Where(&models.Quality{
				Encoding:   false,
				Publishing: false,
				Ready:      false,
				Failed:     false,
			}, "Encoding", "Publishing", "Ready", "Failed").
			Order("id ASC").
			Limit(10).
			Find(&encodingQualitys).Error; err != nil {
			log.Printf("[Services->Encoder->loadEncodingTasks] [ERROR]: Failed to query queued video qualities: %v", err)
			return
		}
	}

	if len(encodingQualitys) > 0 {
		log.Printf("[Services->Encoder->loadEncodingTasks] [INFO]: Loaded %v qualitys to encode", len(encodingQualitys))
	}

	for _, v := range encodingQualitys {
		v.Encoding = true
		if err := v.Save(w.deps.DB).Error; err != nil {
			log.Printf("[Services->Encoder->loadEncodingTasks] [ERROR]: Failed to claim video quality %d: %v", v.ID, err)
			continue
		}
		encodingTasks = append(encodingTasks, EncodingTask{
			Type:   "quality",
			FileID: v.FileID,
			ID:     v.ID,
		})
	}

	// RUNNING ENCODING TASKS
	for _, v := range encodingTasks {
		select {
		case <-ctx.Done():
			return
		case w.limitChan <- true:
		}
		go func(encodingTask EncodingTask) {
			defer func() {
				<-w.limitChan
			}()
			w.runEncode(ctx, encodingTask)
		}(v)
	}
}

func (w *WorkerGroup) runEncode(ctx context.Context, encodingTaskInformation EncodingTask) {
	switch encodingTaskInformation.Type {
	case "quality":
		var encodingTask models.Quality
		w.deps.DB.Preload("File").Find(&encodingTask, encodingTaskInformation.ID)
		w.runEncodeQuality(ctx, encodingTask)
	case "audio":
		var encodingTask models.Audio
		w.deps.DB.Preload("File").Find(&encodingTask, encodingTaskInformation.ID)
		w.runEncodeAudio(ctx, encodingTask)
	case "sub":
		var encodingTask models.Subtitle
		w.deps.DB.Preload("File").Find(&encodingTask, encodingTaskInformation.ID)
		w.runEncodeSub(ctx, encodingTask)
	}
}

func (w *WorkerGroup) runEncodeQuality(ctx context.Context, encodingTask models.Quality) {
	// Explicitly re-query and reload the parent File relation data structures natively
	// to ensure all path variables are fully populated after queue resets!
	var activeFile models.File
	if errFile := w.deps.DB.Where("id = ?", encodingTask.FileID).First(&activeFile).Error; errFile == nil {
		encodingTask.File = activeFile
		//log.Printf("[Services->Encoder->runEncodeQuality] [SUCCESS] Successfully reloaded master video file: %s", encodingTask.File.Path)
	} else {
		log.Printf("[Services->Encoder->runEncodeQuality] [ERROR]: Failed to load parent file metadata for FileID: %d, Error: %v", encodingTask.FileID, errFile)
		encodingTask.Ready = false
		encodingTask.Encoding = false
		encodingTask.Failed = true
		encodingTask.Error = "Failed to resolve master parent file record row properties"
		w.deps.DB.Save(&encodingTask)
		return
	}

	// Always encode to local staging, even if this asset was previously published
	// and its persisted path currently points at VPS3.
	encodingTask.Path = filepath.Join(w.Config().FolderVideoQualitysPriv, encodingTask.File.UUID, filepath.Base(encodingTask.Path))
	if err := w.deps.DB.Model(&models.Quality{}).Where("id = ?", encodingTask.ID).Update("path", encodingTask.Path).Error; err != nil {
		log.Printf("[Services->Encoder->runEncodeQuality] [ERROR]: Failed to persist local staging path for quality %d: %v", encodingTask.ID, err)
		return
	}

	// we check if the original file has been deleted during the waittime
	if !w.originalFileExists(encodingTask.FileID) {
		encodingTask.Ready = false
		encodingTask.Encoding = false
		encodingTask.Failed = true
		encodingTask.Error = "Skipped because waiting for deletion"
		w.deps.DB.Save(&encodingTask)
		return
	}

	// log.Printf("Start encoding %s %s\n", encodingTask.File.UUID, encodingTask.Name)

	totalDuration := encodingTask.File.Duration
	os.MkdirAll(encodingTask.Path, 0777)

	var frameRateString string
	var segmenDuration int = 4
	if encodingTask.AvgFrameRate > 0 {
		frameRateString = fmt.Sprintf("-r %.4f", encodingTask.AvgFrameRate)
	}

	absFileInput, _ := filepath.Abs(encodingTask.File.Path)
	absFolderOutput, _ := filepath.Abs(encodingTask.Path)
	encFilePath := fmt.Sprintf("%s/%s", absFolderOutput, encodingTask.OutputFile)

	// 🌟 Prepare hard-burn subtitle
	var videoFilterArgs string = fmt.Sprintf("-s %dx%d", encodingTask.Width, encodingTask.Height)

	if encodingTask.File.SubtitleMode == "hard" {
		var burnSubtitle models.Subtitle
		err := w.deps.DB.Where("file_id = ? AND selected_for_burn = ?", encodingTask.FileID, true).First(&burnSubtitle).Error
		if err == nil && burnSubtitle.Path != "" && burnSubtitle.OutputFile != "" {

			// Combine the clean directory path column with the exact outputFile name
			// so FFmpeg targets the true physical text file, not an empty directory shell!
			rawSubFilePath := filepath.Join(burnSubtitle.Path, burnSubtitle.OutputFile)
			absSubPath, subErr := filepath.Abs(rawSubFilePath)

			if subErr == nil {
				// 💡 WSL & CROSS-PLATFORM PATH ESCAPING PARITY ENGINE:
				// Protect colons, quotes, and normalize slash directions so FFmpeg
				// parses the absolute disk path successfully across Linux/Windows mounts!
				escapedSubPath := strings.ReplaceAll(absSubPath, "\\", "/")
				escapedSubPath = strings.ReplaceAll(escapedSubPath, "'", "'\\\\''")
				escapedSubPath = strings.ReplaceAll(escapedSubPath, ":", "\\:")

				// Append the subtitle overlay rendering parameter natively into the video filter pipeline
				videoFilterArgs = fmt.Sprintf("-vf \"scale=%d:%d,subtitles='%s'\"", encodingTask.Width, encodingTask.Height, escapedSubPath)

				log.Printf("[Services->Encoder->runEncodeQuality] [SUCCESS] Hard burn target successfully set to: %s", escapedSubPath)
			}
		} else {
			log.Printf("[Services->Encoder->runEncodeQuality] [ERROR] Hard burn requested but no valid ready subtitle was linked to File ID: %d", encodingTask.FileID)
		}
	} else {
		// Default standard clean scaling argument if no hard-burn is selected
		videoFilterArgs = fmt.Sprintf("-s %dx%d", encodingTask.Width, encodingTask.Height)
	}

	// Set preset
	preset := "veryfast" // Default very fast preset for standard video
	if strings.Contains(videoFilterArgs, "-vf") {
		preset = "ultrafast" // Drop to ultrafast to compensate for single-threaded subtitle rendering bottleneck
	}

	// Set threads cap to limit CPU usage, leave 2 cores for main app
	threadsPerJob := w.calculateThreadsPerJob()

	var ffmpegCommand string = "echo Encoding type didnt match && exit 1"
	switch encodingTask.Type {
	case "hls":

		ffmpegCommand = "ffmpeg " +
			fmt.Sprintf("-i %s ", absFileInput) + // input file
			fmt.Sprintf("-threads %d ", threadsPerJob) + // 🌟 CPU Limit Cap
			fmt.Sprint("-sn ") + // disable subtitle
			fmt.Sprint("-an ") + // disable audio
			fmt.Sprint("-c:v libx264 ") + // setting video codec libx264
			fmt.Sprintf("-preset %s ", preset) + // 🌟 Dynamically injected preset
			fmt.Sprintf("-profile:v %s ", encodingTask.Profile) +
			fmt.Sprintf("-level:v %s ", encodingTask.Level) +
			fmt.Sprint("-pix_fmt yuv420p ") + // YUV 4:2:0
			fmt.Sprintf("-crf %d ", encodingTask.Crf) + // setting crf
			fmt.Sprintf("-maxrate %s ", encodingTask.VideoBitrate) + // setting max video bitrate
			fmt.Sprintf("-bufsize %sk ", strconv.Itoa(extractNumber(encodingTask.VideoBitrate)*2)) + // setting video bufsize
			fmt.Sprintf("%s ", frameRateString) + // (optional) setting framerate
			fmt.Sprintf("-force_key_frames \"expr:gte(t,n_forced*%d)\" ", segmenDuration) + // force keyframes every segmentDuration
			"-flags +cgop " + // closed GOP
			fmt.Sprintf("%s ", videoFilterArgs) + // 🌟 Inject subtitle burn argument
			fmt.Sprint("-sc_threshold 0 ") +
			"-f hls " + // hls playlist
			fmt.Sprintf("-hls_time %d ", segmenDuration) + // segment duration
			fmt.Sprint("-hls_playlist_type vod ") +
			fmt.Sprint("-hls_segment_type mpegts ") +
			fmt.Sprint("-hls_list_size 0 ") +
			"-hls_flags independent_segments " + // signals that segments can be decoded independently
			fmt.Sprint("-start_number 0 ") + // start number
			fmt.Sprintf("%s ", encFilePath) + // output file
			fmt.Sprintf("-progress unix://%s -y", w.tempSock(
				totalDuration,
				fmt.Sprintf("%x", sha256.Sum256([]byte(uuid.NewString()))),
				&encodingTask,
			)) // progress tracking
	}

	cmd := exec.CommandContext(ctx,
		"bash",
		"-c",
		ffmpegCommand)

	activeEncodingChannel := make(chan bool)
	defer w.deleteActiveEncoding(encodingTask.FileID, encodingTask.ID, "quality")

	w.addActiveEncoding(ActiveEncoding{
		Type:    "quality",
		FileID:  encodingTask.FileID,
		ID:      encodingTask.ID,
		Channel: &activeEncodingChannel,
	})
	go func() {
		for {
			_, ok := <-activeEncodingChannel
			if !ok {
				break
			}
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			log.Printf("[Services->Encoder->runEncodeQuality] [INFO]: Killed encode (quality) of FileID %d QualityID %d\n", encodingTask.FileID, encodingTask.ID)
		}
	}()

	start := time.Now()
	if err := cmd.Run(); err != nil {
		encodingTask.Ready = false
		encodingTask.Encoding = false
		encodingTask.Failed = true
		w.deps.DB.Save(&encodingTask)
		log.Printf("[Services->Encoder->runEncodeQuality] [ERROR]: Error happened while encoding quality: %v", err.Error())
		log.Println(ffmpegCommand)
		return
	}
	duration := time.Since(start).Seconds()
	w.logic.TrackEncoding(encodingTask.File.UserID, encodingTask.FileID, "quality", duration)

	qualitySize, err := dirSize(absFolderOutput)
	if err != nil {
		log.Printf("[Services->Encoder->runEncodeQuality] [ERROR]: Failed to calc folder size after quality encode: %v", err)
	}

	encodingTask.Size = qualitySize
	encodingTask.Encoding = false
	encodingTask.Publishing = true
	encodingTask.Ready = false
	w.deps.DB.Save(&encodingTask)
}

func (w *WorkerGroup) runEncodeAudio(ctx context.Context, encodingTask models.Audio) {
	// Re-encodes must always target local staging, never the permanent NFS root.
	encodingTask.Path = filepath.Join(w.Config().FolderVideoQualitysPriv, encodingTask.File.UUID, filepath.Base(encodingTask.Path))
	if err := w.deps.DB.Model(&models.Audio{}).Where("id = ?", encodingTask.ID).Update("path", encodingTask.Path).Error; err != nil {
		log.Printf("[Services->Encoder->runEncodeAudio] [ERROR]: Failed to persist local staging path for audio %d: %v", encodingTask.ID, err)
		return
	}

	// we check if the original file has been deleted during the waittime
	if !w.originalFileExists(encodingTask.FileID) {
		encodingTask.Ready = false
		encodingTask.Encoding = false
		encodingTask.Failed = true
		encodingTask.Error = "Skipped because waiting for deletion"
		w.deps.DB.Save(&encodingTask)
		return
	}

	// log.Printf("Start encoding %s %s\n", encodingTask.File.UUID, encodingTask.Name)

	totalDuration := encodingTask.File.Duration
	os.MkdirAll(encodingTask.Path, 0777)

	absFileInput, _ := filepath.Abs(encodingTask.File.Path)
	absFolderOutput, _ := filepath.Abs(encodingTask.Path)

	// Set threads cap to limit CPU usage, leave 2 cores for main app
	threadsPerJob := w.calculateThreadsPerJob()

	var ffmpegCommand string = "echo Audioencoding type didnt match && exit 1"
	switch encodingTask.Type {
	case "hls":

		segmenDuration := 4

		ffmpegCommand = "ffmpeg " +
			fmt.Sprintf("-i %s ", absFileInput) + // input file
			fmt.Sprintf("-threads %d ", threadsPerJob) + // 🌟 Set CPU core limit
			"-sn " + // disable subtitle
			"-vn " + // disable video stream
			fmt.Sprintf("-map 0:a:%d ", encodingTask.Index) + // mapping first audio stream
			`-af aformat=channel_layouts="7.1|5.1|stereo" ` +
			fmt.Sprintf("-c:a %s ", encodingTask.Codec) + // setting audio codec
			"-f hls " + // hls playlist
			fmt.Sprintf("-hls_time %d ", segmenDuration) + // segment duration
			fmt.Sprint("-hls_playlist_type vod ") +
			fmt.Sprint("-hls_segment_type mpegts ") +
			fmt.Sprint("-hls_list_size 0 ") +
			fmt.Sprint("-start_number 0 ") + // start number
			fmt.Sprintf("%s/%s ", absFolderOutput, encodingTask.OutputFile) + // output file
			fmt.Sprintf("-progress unix://%s -y", w.tempSock(
				totalDuration,
				fmt.Sprintf("%x", sha256.Sum256([]byte(uuid.NewString()))),
				&encodingTask,
			)) // progress tracking
	}

	cmd := exec.CommandContext(ctx,
		"bash",
		"-c",
		ffmpegCommand)

	activeEncodingChannel := make(chan bool)
	defer w.deleteActiveEncoding(encodingTask.FileID, encodingTask.ID, "audio")

	w.addActiveEncoding(ActiveEncoding{
		Type:    "audio",
		FileID:  encodingTask.FileID,
		ID:      encodingTask.ID,
		Channel: &activeEncodingChannel,
	})
	go func() {
		for {
			_, ok := <-activeEncodingChannel
			if !ok {
				break
			}
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			log.Printf("[Services->Encoder->runEncodeAudio] [INFO]: Killed encode (audio) of FileID %d AudioID %d\n", encodingTask.FileID, encodingTask.ID)
		}
	}()

	start := time.Now()
	if err := cmd.Run(); err != nil {
		encodingTask.Ready = false
		encodingTask.Encoding = false
		encodingTask.Failed = true
		w.deps.DB.Save(&encodingTask)
		log.Printf("Error happend while encoding audio: %v", err.Error())
		log.Println(ffmpegCommand)
		return
	}
	duration := time.Since(start).Seconds()
	w.logic.TrackEncoding(encodingTask.File.UserID, encodingTask.FileID, "audio", duration)

	encodingTask.Encoding = false
	encodingTask.Publishing = true
	encodingTask.Ready = false
	w.deps.DB.Save(&encodingTask)
}

func (w *WorkerGroup) runEncodeSub(ctx context.Context, encodingTask models.Subtitle) {

	// 🌟 Exclude external subtitles from FFmpeg stream extractions.
	if encodingTask.Source == "external" {
		encodingTask.Encoding = false
		encodingTask.Publishing = true
		encodingTask.Ready = false
		encodingTask.Failed = false
		encodingTask.Progress = 0.0 // It is waiting for file-level publication, not encoding.
		encodingTask.Error = ""

		w.deps.DB.Save(&encodingTask)
		//log.Printf("[Services->Encoder->runEncodeSub] Auto exclude external soft subtitle track UUID: %s", encodingTask.UUID)
		return
	}

	// Re-encoded subtitle output is written to local staging. External subtitles
	// are already stored in the published tree and return above.
	encodingTask.Path = filepath.Join(w.Config().FolderVideoQualitysPriv, encodingTask.File.UUID, filepath.Base(encodingTask.Path))
	if err := w.deps.DB.Model(&models.Subtitle{}).Where("id = ?", encodingTask.ID).Update("path", encodingTask.Path).Error; err != nil {
		log.Printf("[Services->Encoder->runEncodeSub] [ERROR]: Failed to persist local staging path for subtitle %d: %v", encodingTask.ID, err)
		return
	}

	// we check if the original file has been deleted during the waittime
	if !w.originalFileExists(encodingTask.FileID) {
		encodingTask.Ready = false
		encodingTask.Encoding = false
		encodingTask.Failed = true
		encodingTask.Error = "Skipped because waiting for deletion"
		w.deps.DB.Save(&encodingTask)
		return
	}

	// log.Printf("Start encoding %s %s\n", encodingTask.File.UUID, encodingTask.Name)

	totalDuration := encodingTask.File.Duration
	os.MkdirAll(encodingTask.Path, 0777)

	absFileInput, _ := filepath.Abs(encodingTask.File.Path)
	absFolderOutput, _ := filepath.Abs(encodingTask.Path)

	// Set threads cap to limit CPU usage, leave 2 cores for main app
	threadsPerJob := w.calculateThreadsPerJob()

	var ffmpegCommand string = "echo Subencoding type didnt match && exit 1"

	if encodingTask.OriginalCodec == "hdmv_pgs_subtitle" {
		cfg := w.Config()
		if cfg.EnablePluginPgsServer == nil || *cfg.EnablePluginPgsServer == false {
			log.Printf("PluginPgsServer disabled")
			encodingTask.Ready = false
			encodingTask.Encoding = false
			encodingTask.Failed = true
			w.deps.DB.Save(&encodingTask)
			return
		}

		// prepocess pgs
		if err := w.prepocessPgs(ctx, encodingTask, absFolderOutput, &absFileInput); err != nil {
			log.Printf("[Preprocess Error] %v", err)
			encodingTask.Ready = false
			encodingTask.Encoding = false
			encodingTask.Failed = true
			w.deps.DB.Save(&encodingTask)
			return
		}
		defer os.Remove(absFileInput) // delete srt file after encode

		switch encodingTask.Type {
		case "ass":
			ffmpegCommand = "ffmpeg " +
				fmt.Sprintf("-i %s ", absFileInput) + // input file
				fmt.Sprintf("-threads %d ", threadsPerJob) + // 🌟 Set CPU core limit
				fmt.Sprintf("-c:s %s ", encodingTask.Codec) + // setting audio codec
				fmt.Sprintf("%s/%s ", absFolderOutput, encodingTask.OutputFile) + // output file
				fmt.Sprintf("-progress unix://%s -y", w.tempSock(
					totalDuration,
					fmt.Sprintf("%x", sha256.Sum256([]byte(uuid.NewString()))),
					&encodingTask,
				)) // progress tracking
		case "vtt":
			ffmpegCommand = "ffmpeg " +
				fmt.Sprintf("-i %s ", absFileInput) + // input file
				fmt.Sprintf("-threads %d ", threadsPerJob) + // 🌟 Set CPU core limit
				fmt.Sprintf("-c:s %s ", encodingTask.Codec) + // setting audio codec
				fmt.Sprintf("%s/%s ", absFolderOutput, encodingTask.OutputFile) + // output file
				fmt.Sprintf("-progress unix://%s -y", w.tempSock(
					totalDuration,
					fmt.Sprintf("%x", sha256.Sum256([]byte(uuid.NewString()))),
					&encodingTask,
				)) // progress tracking
		}
	} else {
		// normal subtitles
		switch encodingTask.Type {
		case "ass":
			ffmpegCommand = "ffmpeg " +
				fmt.Sprintf("-i %s ", absFileInput) + // input file
				fmt.Sprintf("-threads %d ", threadsPerJob) + // 🌟 Set CPU core limit
				"-an " + // disable audio
				"-vn " + // disable video stream
				fmt.Sprintf("-map 0:s:%d ", encodingTask.Index) + // mapping first audio stream
				fmt.Sprintf("-c:s %s ", encodingTask.Codec) + // setting audio codec
				fmt.Sprintf("%s/%s ", absFolderOutput, encodingTask.OutputFile) + // output file
				fmt.Sprintf("-progress unix://%s -y", w.tempSock(
					totalDuration,
					fmt.Sprintf("%x", sha256.Sum256([]byte(uuid.NewString()))),
					&encodingTask,
				)) // progress tracking
		case "vtt":
			ffmpegCommand = "ffmpeg " +
				fmt.Sprintf("-i %s ", absFileInput) + // input file
				fmt.Sprintf("-threads %d ", threadsPerJob) + // 🌟 Set CPU core limit
				"-an " + // disable audio
				"-vn " + // disable video stream
				fmt.Sprintf("-map 0:s:%d ", encodingTask.Index) + // mapping first audio stream
				fmt.Sprintf("-c:s %s ", encodingTask.Codec) + // setting audio codec
				fmt.Sprintf("%s/%s ", absFolderOutput, encodingTask.OutputFile) + // output file
				fmt.Sprintf("-progress unix://%s -y", w.tempSock(
					totalDuration,
					fmt.Sprintf("%x", sha256.Sum256([]byte(uuid.NewString()))),
					&encodingTask,
				)) // progress tracking
		}
	}

	cmd := exec.CommandContext(ctx,
		"bash",
		"-c",
		ffmpegCommand)
	activeEncodingChannel := make(chan bool)
	defer w.deleteActiveEncoding(encodingTask.FileID, encodingTask.ID, "sub")

	w.addActiveEncoding(ActiveEncoding{
		Type:    "sub",
		FileID:  encodingTask.FileID,
		ID:      encodingTask.ID,
		Channel: &activeEncodingChannel,
	})
	go func() {
		for {
			_, ok := <-activeEncodingChannel
			if !ok {
				break
			}
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			log.Printf("[Services->Encoder->runEncodeSub] [INFO]: Killed encode (subtitle) of FileID %d SubID %d\n", encodingTask.FileID, encodingTask.ID)
		}
	}()

	start := time.Now()
	if err := cmd.Run(); err != nil {
		encodingTask.Ready = false
		encodingTask.Encoding = false
		encodingTask.Failed = true
		w.deps.DB.Save(&encodingTask)
		log.Printf("[Services->Encoder->runEncodeSub] [ERROR]: Error happened while encoding subtitle: %v", err.Error())
		log.Println(ffmpegCommand)
		return
	}
	duration := time.Since(start).Seconds()
	w.logic.TrackEncoding(encodingTask.File.UserID, encodingTask.FileID, "sub", duration)

	encodingTask.Encoding = false
	encodingTask.Publishing = true
	encodingTask.Ready = false
	w.deps.DB.Save(&encodingTask)
}

func (w *WorkerGroup) calculateThreadsPerJob() int {
	totalCores := runtime.NumCPU()

	// 1. Keep 25% of the machine's capacity free for Web/Player traffic, capped at 2 cores max
	freeCoresForWeb := totalCores / 4
	if freeCoresForWeb > 2 {
		freeCoresForWeb = 2
	}
	// Safeguard: Ensure we leave at least 1 core for the OS/Go if totalCores > 1
	if freeCoresForWeb < 1 && totalCores > 1 {
		freeCoresForWeb = 1
	}

	// 2. Divide remaining cores among allowed concurrent encoding queues
	maxJobs := int(w.Config().MaxRunningEncodes)
	if maxJobs < 1 {
		maxJobs = 1
	}

	threadsPerJob := (totalCores - freeCoresForWeb) / maxJobs

	// 3. Absolute safety fallback: An FFmpeg job must use at least 1 thread
	if threadsPerJob < 1 {
		threadsPerJob = 1
	}

	return threadsPerJob
}

func (w *WorkerGroup) tempSock(totalDuration float64, sockFileName string, encodingTask IwithProcess) string {
	sockFilePath := path.Join(os.TempDir(), sockFileName)
	_ = os.Remove(sockFilePath)
	l, err := net.Listen("unix", sockFilePath)
	if err != nil {
		log.Printf("failed to create encoder progress socket %s: %v", sockFilePath, err)
		return sockFilePath
	}

	go func() {
		defer l.Close()
		re := regexp.MustCompile(`out_time_ms=(\d+)`)
		fd, err := l.Accept()
		if err != nil {
			log.Printf("[Services->Encoder->tempSock] [ERROR]: Encoder progress socket accept error: %v", err)
			return
		}
		defer fd.Close()
		buf := make([]byte, 16)
		data := ""
		progress := ""
		for {
			_, err := fd.Read(buf)
			if err != nil {
				return
			}
			data += string(buf)
			a := re.FindAllStringSubmatch(data, -1)
			cp := ""
			if len(a) > 0 && len(a[len(a)-1]) > 0 {
				c, _ := strconv.Atoi(a[len(a)-1][len(a[len(a)-1])-1])
				cp = fmt.Sprintf("%.2f", float64(c)/totalDuration/1000000)
			}
			if strings.Contains(data, "progress=end") {
				cp = "1.0"
			}
			if cp == "" {
				cp = ".0"
			}
			if cp != progress {
				progress = cp
				// fmt.Println("progress: ", progress)
				floatProg, err := strconv.ParseFloat(progress, 64)
				if err != nil {
					log.Printf("[Services->Encoder->tempSock] [ERROR]: Could not save progress in database")
				}
				if floatProg != 0 {
					encodingTask.SetProcess(floatProg)
				}
				encodingTask.Save(w.deps.DB)
			}
		}
	}()

	return sockFilePath
}

func (w *WorkerGroup) originalFileExists(fileId uint) bool {
	if res := w.deps.DB.First(&models.File{}, fileId); res.Error != nil {
		return false
	}
	return true
}

func (w *WorkerGroup) addActiveEncoding(encoding ActiveEncoding) {
	w.activeEncodingsMu.Lock()
	w.activeEncodings = append(w.activeEncodings, encoding)
	w.activeEncodingsMu.Unlock()
}

func (w *WorkerGroup) activeEncodingsForFile(fileID uint) []ActiveEncoding {
	w.activeEncodingsMu.Lock()
	defer w.activeEncodingsMu.Unlock()

	encodings := make([]ActiveEncoding, 0)
	for _, encoding := range w.activeEncodings {
		if encoding.FileID == fileID {
			encodings = append(encodings, encoding)
		}
	}
	return encodings
}

func (w *WorkerGroup) deleteActiveEncoding(fileID uint, ID uint, Type string) {
	w.activeEncodingsMu.Lock()
	defer w.activeEncodingsMu.Unlock()

	foundIndex := -1
	for i, v := range w.activeEncodings {
		if v.FileID == fileID && v.ID == ID && v.Type == Type {
			foundIndex = i
		}
	}
	if foundIndex < 0 {
		log.Printf("[Services->Encoder->deleteActiveEncoding] [ERROR]: Failed to delete an active encoding with the fileID %v and ID %v", fileID, ID)
		return
	}

	w.activeEncodings = removeFromArray(w.activeEncodings, foundIndex)
}

func (w *WorkerGroup) prepocessPgs(ctx context.Context, encodingTask models.Subtitle, absFolderOutput string, absFileInput *string) error {

	ffmpegOutputFile := fmt.Sprintf("%s.sup", encodingTask.OutputFile)
	ffmpegOutputFilePath := fmt.Sprintf("%s/%s", absFolderOutput, ffmpegOutputFile)
	pgsOutputFilePath := fmt.Sprintf("%s/%s.srt", absFolderOutput, encodingTask.OutputFile)
	defer os.Remove(ffmpegOutputFilePath)

	ffmpegCommand := "ffmpeg -y " +
		fmt.Sprintf("-i %s ", *absFileInput) + // input file
		"-an " + // disable audio
		"-vn " + // disable video stream
		fmt.Sprintf("-map 0:s:%d ", encodingTask.Index) + // mapping first audio stream
		fmt.Sprintf("-c:s copy ") + // setting audio codec
		ffmpegOutputFilePath // output file progress

	// convert to srt
	cmd := exec.CommandContext(ctx,
		"bash",
		"-c",
		ffmpegCommand)
	if err := cmd.Run(); err != nil {
		log.Println(ffmpegCommand)
		return fmt.Errorf("[Services->Encoder->prepocessPgs] [ERROR]: Error happened while encoding subtitle: %v", err.Error())
	}

	pgsFile, err := os.Open(ffmpegOutputFilePath)
	if err != nil {
		return fmt.Errorf("[Services->Encoder->prepocessPgs] [ERROR]: Error happened while opening pgs subtitle: %v", err.Error())
	}

	requestCtx, cancel := context.WithTimeout(ctx, time.Minute*5)
	defer cancel()

	client := req.C()
	res, err := client.R().
		SetContext(requestCtx).
		SetFileReader("file", "subtitle.sup", pgsFile).
		Post(w.Config().PluginPgsServer)
	if err != nil {
		return fmt.Errorf("[Services->Encoder->prepocessPgs] [ERROR]: Error happened while scanning pgs subtitle: %v", err.Error())
	}
	if !res.IsSuccessState() {
		return fmt.Errorf("[Services->Encoder->prepocessPgs] [ERROR]: Error happened waiting for srt from pgs plugin: %v", err)
	}
	if err := os.WriteFile(pgsOutputFilePath, res.Bytes(), 0644); err != nil {
		return fmt.Errorf("[Services->Encoder->prepocessPgs] [ERROR]: Error happened while saving srt from pgs: %v", err.Error())
	}
	*absFileInput = pgsOutputFilePath
	return nil
}

func extractNumber(input string) int {
	re := regexp.MustCompile(`[-]?\d[\d,]*[\.]?[\d{2}]*`)
	submatchall := re.FindAllString(input, -1)
	if len(submatchall) > 0 {
		if i, err := strconv.Atoi(submatchall[0]); err == nil {
			return i
		}
	}
	return 0
}

func dirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return err
	})
	return size, err
}

func removeFromArray[T any](s []T, i int) []T {
	if len(s) == 0 || len(s) <= i || i < 0 {
		return s
	}
	s[i] = s[len(s)-1]
	return s[:len(s)-1]
}

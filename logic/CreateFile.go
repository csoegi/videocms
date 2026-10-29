package logic

import (
	"ch/kirari04/videocms/config"
	"ch/kirari04/videocms/helpers"
	"ch/kirari04/videocms/models"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gopkg.in/vansante/go-ffprobe.v2"
	"gorm.io/gorm"
)

func (s *Service) CreateFile(
	fromFile *string,
	toFolder uint,
	fileName string,
	fileId string,
	fileSize int64,
	userId uint,
	excludeSessionUUID string,
	subtitleMode string,
	externalSubs []models.Subtitle,
) (status int, newFile *models.Link, cloned bool, err error) {
	return s.createFile(fromFile, toFolder, fileName, fileId, fileSize, userId, excludeSessionUUID, subtitleMode, externalSubs, false)
}

// CreateFileDirect registers an already-packaged media file without sending it
// through the HLS encoder. It is used for HLS URLs that have been remuxed to MP4.
func (s *Service) CreateFileDirect(
	fromFile *string,
	toFolder uint,
	fileName string,
	fileId string,
	fileSize int64,
	userId uint,
	excludeSessionUUID string,
	subtitleMode string,
	externalSubs []models.Subtitle,
) (status int, newFile *models.Link, cloned bool, err error) {
	return s.createFile(fromFile, toFolder, fileName, fileId, fileSize, userId, excludeSessionUUID, subtitleMode, externalSubs, true)
}

func (s *Service) createFile(
	fromFile *string,
	toFolder uint,
	fileName string,
	fileId string,
	fileSize int64,
	userId uint,
	excludeSessionUUID string,
	subtitleMode string,
	externalSubs []models.Subtitle,
	forceDirect bool,
) (status int, newFile *models.Link, cloned bool, err error) {
	// Keep the display title separate from the uploaded media's file extension.
	// The extension is inferred from the actual media path/content when needed.
	if ext := filepath.Ext(fileName); ext != "" {
		fileName = strings.TrimSuffix(fileName, ext)
	}
	// Direct assets are staged under FolderVideoQualitysPriv/<file UUID> and
	// published by the existing publisher. If database setup fails part-way
	// through, remove that newly-created staging tree rather than leave an
	// orphaned direct-media file behind.
	directStageRoot := ""
	directArchivePath := ""
	directSetupComplete := false
	defer func() {
		if directStageRoot != "" && !directSetupComplete {
			if err := os.RemoveAll(directStageRoot); err != nil {
				log.Printf("[Logic->CreateFile] Failed to clean incomplete direct-media staging directory %s: %v", directStageRoot, err)
			}
		}
		if directArchivePath != "" && !directSetupComplete {
			if err := os.Remove(directArchivePath); err != nil && !os.IsNotExist(err) {
				log.Printf("[Logic->CreateFile] Failed to remove archive for incomplete direct-media registration %s: %v", directArchivePath, err)
			}
		}
	}()

	//check if requested folder exists (if set)
	if toFolder > 0 {
		res := s.Deps.DB.First(&models.Folder{}, toFolder)
		if res.Error != nil {
			return http.StatusBadRequest, nil, false, errors.New("parent folder doesn't exist")
		}
	}

	// obtain hash from file
	FileHash, err := helpers.HashFile(*fromFile)
	if err != nil {
		log.Printf("Failed to create hash from file: %v", err)
		return http.StatusInternalServerError, nil, false, echo.ErrInternalServerError
	}

	// check file hash with database
	status, newLink, err := s.CloneFileByHash(FileHash, toFolder, fileName, userId, excludeSessionUUID)
	if err == nil {
		return status, newLink, true, err
	}

	// check storage quota for new file
	if status, err := s.CheckStorageQuota(userId, fileSize, excludeSessionUUID); err != nil {
		return status, nil, false, err
	}

	// run file through ffmpeg so the metadata is more accurate
	nameSplits := strings.Split(fileName, ".")
	fileExt := nameSplits[len(nameSplits)-1]
	oldOutPath := *fromFile
	newOutPath := fmt.Sprintf("%s.%s", *fromFile, fileExt)
	fromFile = &newOutPath

	// check if file extension is supported
	fileExt = strings.ToLower(fileExt)
	if !slices.Contains(config.EXTENSIONS, fileExt) {
		return http.StatusBadRequest, nil, false, errors.New("Video extension is not supported")
	}

	// Preserve the existing encoding pipeline when encoding is enabled. When
	// it is disabled, only MP4/MKV can be published as direct media in this
	// initial implementation; other source formats need transcoding first.
	encodingEnabled := s.Config().EncodingEnabled != nil && *s.Config().EncodingEnabled
	directMode := forceDirect || !encodingEnabled
	directContainer := strings.ToLower(fileExt)
	if directMode && directContainer != "mp4" && directContainer != "mkv" {
		return http.StatusBadRequest, nil, false, errors.New("When encoding is disabled, only MP4 and MKV files can be uploaded or remotely downloaded")
	}

	// Snapshot valid HLS output profiles before moving/registering the source.
	// The normal encoding path must never create audio/subtitle jobs without at
	// least one video quality job to satisfy the publisher's file-level gate.
	var enabledHLSProfiles []models.AvailableQuality
	if !directMode {
		for _, profile := range s.Qualities() {
			if profile.Enabled && strings.EqualFold(profile.Type, "hls") && profile.Width > 0 && profile.Height > 0 && profile.OutputFile != "" {
				enabledHLSProfiles = append(enabledHLSProfiles, profile)
			}
		}
		if len(enabledHLSProfiles) == 0 {
			return http.StatusBadRequest, nil, false, errors.New("encoding is enabled, but no valid HLS output profiles are enabled")
		}
	}

	if err := os.Rename(oldOutPath, newOutPath); err != nil {
		log.Printf("Failed to delete oldInputEncoding File %s: %v\n", oldOutPath, err.Error())
	}

	// ffprobe context
	ctx, cancelFn := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFn()

	// probe file
	data, err := ffprobe.ProbeURL(ctx, *fromFile)
	if err != nil {
		// log.Printf("Error getting data using ffprobe: %v", err)
		// return http.StatusInternalServerError, nil, false, echo.ErrInternalServerError
		return http.StatusBadRequest, nil, false, errors.New("Invalid data found when processing video")
	}
	// probe type
	dataStreams := data.StreamType(ffprobe.StreamAny)
	// extract internal subtitles from stream
	dataSubtitleStreams := data.StreamType(ffprobe.StreamSubtitle)
	// declare needed informations
	var videoStream ffprobe.Stream
	var subtitleStreams []ffprobe.Stream
	var audioStreams []ffprobe.Stream
	var avgFramerate float64
	videoDuration := data.Format.Duration().Seconds()
	hasVideoStream := false

	if videoDuration == 0 || videoDuration > 60*60*10 {
		return http.StatusBadRequest, nil, false, errors.New("invalid video duration")
	}

	// loop over streams in file
	for _, streamInfo := range dataStreams {
		if streamInfo.CodecType == "video" {
			videoStream = streamInfo
			hasVideoStream = true
		}

		if streamInfo.CodecType == "audio" {
			audioStreams = append(audioStreams, streamInfo)
		}
	}

	//loop over subtitles in file
	for _, streamInfo := range dataSubtitleStreams {
		subtitleStreams = append(subtitleStreams, streamInfo)
	}

	//check if video stream exists
	if !hasVideoStream {
		return http.StatusBadRequest, nil, false, errors.New("uploaded file doesn't contain any video stream")
	}

	// set average framerate
	if rawAvgFramerateObj := strings.Split(videoStream.AvgFrameRate, "/"); len(rawAvgFramerateObj) == 2 {
		a, errA := strconv.ParseFloat(rawAvgFramerateObj[0], 64)
		b, errB := strconv.ParseFloat(rawAvgFramerateObj[1], 64)
		if a > 0 && b > 0 && errA == nil && errB == nil {
			avgFramerate = a / b
		}
	}

	// check average framerate
	if avgFramerate > 120 || avgFramerate < 0 {
		return http.StatusBadRequest, nil, false, errors.New("invalid video framerate")
	}

	// check video stream data (resolution)
	if videoStream.Height == 0 || videoStream.Width == 0 {
		log.Printf(
			"Error getting valid videoStream data: type: %v size: %vx%v",
			videoStream.CodecType,
			videoStream.Width,
			videoStream.Height,
		)
		return http.StatusInternalServerError, nil, false, echo.ErrInternalServerError
	}

	// check if resolution is in scope of supported sizes
	if videoStream.Height > 8000 || videoStream.Width > 8000 {
		return http.StatusBadRequest, nil, false, errors.New("video resolution is too high")
	}
	if videoStream.Height < 50 || videoStream.Width < 50 {
		return http.StatusBadRequest, nil, false, errors.New("video resolution is too low")
	}

	// declare required variables for database insert
	videoHeight := videoStream.Height
	videoWidth := videoStream.Width

	if videoDuration == 0 {
		log.Printf("Error getting videoDuration: %v %v", err, dataStreams)
		return http.StatusInternalServerError, nil, false, echo.ErrInternalServerError
	}

	thumbnailFileName := "4x4.webp"
	if directMode {
		// Direct media must live inside the publisher's staging tree so the
		// existing PublisherTask can move it atomically with the thumbnail and
		// any external subtitles.
		qualityName := fmt.Sprintf("%dp", videoHeight)
		outputFile := "video." + directContainer
		directStageRoot = filepath.Join(s.Config().FolderVideoQualitysPriv, fileId)
		directQualityDir := filepath.Join(directStageRoot, qualityName)
		if err := os.MkdirAll(directQualityDir, 0755); err != nil {
			return http.StatusInternalServerError, nil, false, fmt.Errorf("failed to prepare direct-media staging directory: %w", err)
		}
		directPath := filepath.Join(directQualityDir, outputFile)
		// Preserve a separate archive copy of the original direct MP4/MKV.
		// The published quality copy remains untouched so playback and the
		// existing HLS converter continue to work exactly as before.
		archivePath, archiveErr := archiveDirectMediaFile(*fromFile, s.Config().FolderVideoArchives, fileName, directContainer, fileId)
		if archiveErr != nil {
			return http.StatusInternalServerError, nil, false, fmt.Errorf("failed to archive original direct media: %w", archiveErr)
		}
		directArchivePath = archivePath
		if err := moveFileToPath(*fromFile, directPath); err != nil {
			return http.StatusInternalServerError, nil, false, fmt.Errorf("failed to stage direct media: %w", err)
		}
		fromFile = &directPath
		if info, statErr := os.Stat(directPath); statErr == nil {
			fileSize = info.Size()
		}

		// Generate the thumbnail before the quality is queued for publication;
		// otherwise the publisher could move the staging tree while FFmpeg is
		// still reading the source.
		if avgFramerate > 0 {
			if _, err := s.CreateThumbnail(4, *fromFile, 1080, thumbnailFileName, directStageRoot, videoDuration, avgFramerate); err != nil {
				log.Printf("Failed to generate thumbnail from direct media %s: %v", *fromFile, err)
			}
		}
	} else {
		go func() {
			if avgFramerate > 0 {
				if _, err := s.CreateThumbnail(
					4,
					*fromFile,
					1080,
					thumbnailFileName,
					fmt.Sprintf("%s/%s", s.Config().FolderVideoQualitysPriv, fileId),
					videoDuration,
					avgFramerate,
				); err != nil {
					log.Printf("Failed to generate thumbnail from file %v: %v", fromFile, err)
				}
			}
		}()
	}

	if directMode {
		// Direct uploads don't transcode embedded subtitles. Only explicitly
		// uploaded external subtitles are supported in this mode.
		if len(externalSubs) > 0 {
			subtitleMode = "soft"
		} else {
			subtitleMode = "none"
		}
	}
	var dbFile models.File
	var dbLink models.Link
	// create an transaction consisting of the file and its link
	// a transaction is necessary so the service Deleter wont mark an file as unreferenced by accident
	if err := s.Deps.DB.Transaction(func(tx *gorm.DB) error {
		filePath := *fromFile
		if directMode {
			filePath = filepath.Join(s.Config().PublishedQualitysRoot(), fileId, filepath.Base(filepath.Dir(*fromFile)), filepath.Base(*fromFile))
		}
		dbFile = models.File{
			UUID:         fileId,
			Hash:         FileHash,
			Thumbnail:    thumbnailFileName,
			Path:         filePath,
			Folder:       fmt.Sprintf("%s/%s", s.Config().FolderVideoQualitysPriv, fileId),
			UserID:       userId,
			Height:       int64(videoHeight),
			Width:        int64(videoWidth),
			Duration:     videoDuration,
			AvgFrameRate: avgFramerate,
			Size:         fileSize,
			SubtitleMode: subtitleMode,
		}
		if err := tx.Create(&dbFile).Error; err != nil {
			return err
		}
		dbLink = models.Link{
			UUID:           uuid.NewString(),
			Name:           fileName,
			ParentFolderID: toFolder,
			UserID:         userId,
			FileID:         dbFile.ID,
		}
		if err := tx.Create(&dbLink).Error; err != nil {
			return err
		}
		return nil
	}); err != nil {
		log.Printf("Error saving file & link in database: %v", err)
		return http.StatusInternalServerError, nil, false, echo.ErrInternalServerError
	}

	// === STEP 1: Save internal subtitle to database
	// If the video container contains embedded subtitle track streams,
	// set the video subtitle mode to "soft"
	if !directMode && len(subtitleStreams) > 0 {
		dbFile.SubtitleMode = "soft"
		if errUpdateMode := s.Deps.DB.Model(&dbFile).Update("subtitle_mode", "soft").Error; errUpdateMode != nil {
			log.Printf("[Logic->CreateFile] [ERROR] Failed to set initial video SubtitleMode to soft: %v", errUpdateMode)
		}
	}

	if !directMode {
		for index, subtitleStream := range subtitleStreams {
			// generate subtitle name
			var subtitleName = fmt.Sprintf("Subtitle %v", index+1)
			if autoName := subtitleStream.Tags.Title; autoName != "" && len(autoName) < 20 {
				subtitleName = autoName
			}

			// detect subtitle language
			var subtitleLang = "eng"
			if autoLang := subtitleStream.Tags.Language; autoLang != "" && len(autoLang) < 10 {
				subtitleLang = autoLang
			}

			for _, subOpt := range models.AvailableSubtitles {
				// generate unique identifier for subtitle
				subtitleId := uuid.NewString()

				// save subtitle data to database
				dbSubtitle := models.Subtitle{
					UUID:          subtitleId,
					Name:          subtitleName,
					Lang:          subtitleLang,
					Index:         index, // Set index for internal subtitle required for transcoding
					Codec:         subOpt.Codec,
					Type:          subOpt.Type,
					OutputFile:    subOpt.OutputFile,
					FileID:        dbFile.ID,
					Path:          fmt.Sprintf("%s/%s/%s", s.Config().FolderVideoQualitysPriv, dbFile.UUID, subtitleId),
					OriginalCodec: subtitleStream.CodecName,
					Source:        "internal", // Set to "internal"
					Encoding:      false,
					Failed:        false,
					Ready:         false,
					Error:         "",
				}
				if res := s.Deps.DB.Create(&dbSubtitle); res.Error != nil {
					log.Printf("[Logic->CreateFile] [ERROR] Error saving Internal Subtitle in database: %v", res.Error)
					return http.StatusInternalServerError, nil, false, echo.ErrInternalServerError
				}
			}
		}
	}

	// === STEP 2: Save external subtitles to disk and database
	for _, extSub := range externalSubs {
		nameSplits := strings.Split(extSub.Name, ".")
		ext := "vtt"
		if len(nameSplits) > 1 {
			ext = strings.ToLower(nameSplits[len(nameSplits)-1])
		}

		baseQualityDir := filepath.ToSlash(s.Config().FolderVideoQualitysPriv)
		tempStagingPath := filepath.ToSlash(extSub.Path) // 💡 Now contains the true staging path decoded from JSON!

		// Build the true multi-track subfolder hierarchy targets
		productionSubDir := fmt.Sprintf("%s/%s/%s", baseQualityDir, dbFile.UUID, extSub.UUID)
		targetOutputName := fmt.Sprintf("%s.%s", extSub.UUID, ext)
		finalDiskFilePath := fmt.Sprintf("%s/%s", productionSubDir, targetOutputName)

		if err := os.MkdirAll(productionSubDir, 0755); err != nil {
			log.Printf("[Logic->CreateFile] [ERROR] Failed to create subtitle subdirectory path: %v", err)
			continue
		}

		// Read the temp text asset file cleanly using the decoded path variable string
		subBytes, errRead := os.ReadFile(tempStagingPath)
		if errRead != nil {
			log.Printf("[Logic->CreateFile] [ERROR] Failed to read temporary staging subtitle file track at %s: %v", tempStagingPath, errRead)
			continue
		}

		// Write the processed asset file out directly inside the quality subfolder partition
		if errWrite := os.WriteFile(finalDiskFilePath, subBytes, 0644); errWrite != nil {
			log.Printf("[Logic->CreateFile] [ERROR] Failed to write subtitle file to quality folder: %v", errWrite)
			continue
		}

		// Clean up the temporary upload staging file track asset safely
		_ = os.Remove(tempStagingPath)

		// Update the database records fields
		errUpdate := s.Deps.DB.Model(&models.Subtitle{}).
			Where("uuid = ?", extSub.UUID).
			Updates(map[string]interface{}{
				"file_id":        dbFile.ID,
				"output_file":    targetOutputName,
				"codec":          "vtt",
				"original_codec": ext,
				"source":         "external", // Set to "external"
				"index":          -1,         // Default index for external
				"path":           productionSubDir,
				"ready":          false, // External subtitle is staged until publication
				"publishing":     true,
				"encoding":       false,
				"failed":         false,
				"progress":       0.0, // Waiting for publication is not encoding progress.
			}).Error

		if errUpdate != nil {
			log.Printf("[Logic->CreateFile] [ERROR] Failed saving subtitle: %v", errUpdate)
		} else {
			log.Printf("[Logic->CreateFile] [SUCCESS] Subtitle track %s is successfully saved: %s", extSub.UUID, finalDiskFilePath)
		}
	}

	// === STEP 3: Save audio data only when the encoder will process the source.
	if !directMode {
		for index, audioStream := range audioStreams {
			// generate  audio name
			var audioName = fmt.Sprintf("Audio %v", index+1)
			if autoName, err := audioStream.TagList.GetString("title"); err == nil && autoName != "" {
				audioName = autoName
			}
			// detect  audio language
			var audioLang = "en"
			if autoLang, err := audioStream.TagList.GetString("language"); err == nil && autoLang != "" && len(autoLang) < 10 {
				audioLang = autoLang
			}

			// log.Printf(" audioName: %s /  audioLang: %s", audioName, audioLang)

			for _, audioOpt := range models.AvailableAudios {
				// generate unique identifier for  audio
				audioId := uuid.NewString()

				// save  audio data to database
				if res := s.Deps.DB.Create(&models.Audio{
					UUID:          audioId,
					Name:          audioName,
					Lang:          audioLang,
					Index:         index,
					Codec:         audioOpt.Codec,
					Type:          audioOpt.Type,
					OutputFile:    audioOpt.OutputFile,
					FileID:        dbFile.ID,
					Path:          fmt.Sprintf("%s/%s/%s", s.Config().FolderVideoQualitysPriv, dbFile.UUID, audioId),
					OriginalCodec: audioStream.CodecName,
					Encoding:      false,
					Failed:        false,
					Ready:         false,
					Error:         "",
				}); res.Error != nil {
					log.Printf("Error saving Audio in database: %v", res.Error)
					return http.StatusInternalServerError, nil, false, echo.ErrInternalServerError
				}
			}
		}
	}

	// === STEP 4: Save video qualities to database so they can be converted later.
	if directMode {
		qualityName := fmt.Sprintf("%dp", videoHeight)
		outputFile := "video." + directContainer
		qualityPath := filepath.Join(s.Config().FolderVideoQualitysPriv, fileId, qualityName)
		qualitySize := fileSize
		if info, statErr := os.Stat(*fromFile); statErr == nil {
			qualitySize = info.Size()
		}
		if res := s.Deps.DB.Create(&models.Quality{
			FileID: dbFile.ID, Name: qualityName, Width: int64(videoWidth), Height: int64(videoHeight),
			Size: qualitySize, Type: directContainer, Muted: false, AvgFrameRate: avgFramerate,
			Path: qualityPath, OutputFile: outputFile, Encoding: false, Publishing: true,
			Failed: false, Ready: false, Progress: 0, Error: "",
		}).Error; res != nil {
			log.Printf("Error saving direct media quality in database: %v", res)
			return http.StatusInternalServerError, nil, false, echo.ErrInternalServerError
		}
	} else {
		qualityRowsCreated := 0
		var fallbackProfile models.AvailableQuality
		var fallbackArea int64 = -1
		for _, profile := range enabledHLSProfiles {
			area := profile.Width * profile.Height
			if fallbackArea < 0 || area < fallbackArea {
				fallbackProfile, fallbackArea = profile, area
			}
		}

		var qualityFrameRate float64
		if avgFramerate > float64(s.Config().MaxFramerate) {
			qualityFrameRate = float64(s.Config().MaxFramerate)
		}

		for _, profile := range enabledHLSProfiles {
			targetWidth, targetHeight, fits := fitQualityDimensions(int64(videoWidth), int64(videoHeight), profile.Width, profile.Height)
			if !fits {
				continue // Do not upscale a source to a larger profile.
			}
			qualityPath := filepath.Join(s.Config().FolderVideoQualitysPriv, fileId, profile.FolderName)
			quality := models.Quality{
				FileID: dbFile.ID, Name: profile.Name,
				Width: targetWidth, Height: targetHeight,
				VideoBitrate: profile.VideoBitrate, AudioBitrate: profile.AudioBitrate,
				Profile: profile.Profile, Level: profile.Level,
				CodecStringAVC: profile.CodecStringAVC, Crf: profile.Crf,
				Type: "hls", Muted: profile.Muted, AvgFrameRate: qualityFrameRate,
				Path: qualityPath, OutputFile: profile.OutputFile,
				Encoding: false, Publishing: false, Failed: false, Ready: false,
				Progress: 0, Error: "",
			}
			if err := s.Deps.DB.Create(&quality).Error; err != nil {
				log.Printf("Error saving quality in database: %v", err)
				return http.StatusInternalServerError, nil, false, echo.ErrInternalServerError
			}
			qualityRowsCreated++
		}

		// If every configured profile would upscale the source (for example, a
		// 480p upload with only a 720p profile enabled), still create one HLS
		// quality at native resolution. Without this fallback, audio/subtitle jobs
		// can finish but Publisher.fileReadyToPublish waits forever for a quality.
		if qualityRowsCreated == 0 {
			nativeWidth := int64(videoWidth) / 2 * 2
			nativeHeight := int64(videoHeight) / 2 * 2
			qualityName := fmt.Sprintf("%dp", nativeHeight)
			quality := models.Quality{
				FileID: dbFile.ID, Name: qualityName,
				Width: nativeWidth, Height: nativeHeight,
				VideoBitrate: fallbackProfile.VideoBitrate, AudioBitrate: fallbackProfile.AudioBitrate,
				Profile: fallbackProfile.Profile, Level: fallbackProfile.Level,
				CodecStringAVC: fallbackProfile.CodecStringAVC, Crf: fallbackProfile.Crf,
				Type: "hls", Muted: fallbackProfile.Muted, AvgFrameRate: qualityFrameRate,
				Path:       filepath.Join(s.Config().FolderVideoQualitysPriv, fileId, qualityName),
				OutputFile: fallbackProfile.OutputFile,
				Encoding:   false, Publishing: false, Failed: false, Ready: false,
				Progress: 0, Error: "",
			}
			if err := s.Deps.DB.Create(&quality).Error; err != nil {
				log.Printf("Error saving native-resolution fallback quality in database: %v", err)
				return http.StatusInternalServerError, nil, false, echo.ErrInternalServerError
			}
			log.Printf("[Logic->CreateFile] No enabled HLS profile fits source %dx%d without upscaling; queued native-resolution quality %s for file %s", videoWidth, videoHeight, qualityName, fileId)
		}
	}

	// The publisher can now safely consume the direct-media staging tree.
	if directMode {
		directSetupComplete = true
	}

	// return link to file
	return http.StatusOK, &dbLink, false, nil
}


func archiveDirectMediaFile(source, archiveRoot, title, extension, fileID string) (string, error) {
	if strings.TrimSpace(archiveRoot) == "" {
		archiveRoot = "./videos/archives"
	}
	extension = strings.TrimPrefix(strings.ToLower(extension), ".")
	if extension != "mp4" && extension != "mkv" {
		return "", fmt.Errorf("unsupported direct-media archive extension %q", extension)
	}

	// Use the source file's actual extension when available, not the display title.
	if sourceExtension := strings.TrimPrefix(strings.ToLower(filepath.Ext(source)), "."); sourceExtension == "mp4" || sourceExtension == "mkv" {
		extension = sourceExtension
	}

	safeTitle := sanitizeArchiveTitle(trimArchiveContainerMarker(title, extension))
	suffix := strings.ReplaceAll(strings.ToLower(fileID), "-", "")
	if suffix == "" {
		suffix = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	archiveName := fmt.Sprintf("%s_%d_%s.%s", safeTitle, time.Now().UnixMilli(), suffix, extension)
	archivePath := filepath.Join(archiveRoot, archiveName)

	if err := os.MkdirAll(archiveRoot, 0755); err != nil {
		return "", fmt.Errorf("create archive directory %s: %w", archiveRoot, err)
	}
	if err := copyOriginalToArchive(source, archivePath); err != nil {
		return "", fmt.Errorf("copy %s to archive %s: %w", source, archivePath, err)
	}
	log.Printf("[Logic->CreateFile] Archived original direct media: %s", archivePath)
	return archivePath, nil
}

func trimArchiveContainerMarker(title, extension string) string {
	// Some remote-download titles already end with a container marker such as
	// "-mp4" even though the actual file extension is appended separately.
	// Remove only a trailing marker matching the source container.
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

func sanitizeArchiveTitle(title string) string {
	var b strings.Builder
	lastSeparator := false
	for _, r := range strings.TrimSpace(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			piece := string(r)
			// Keep the filename below common 255-byte component limits after
			// adding the timestamp, UUID suffix, separators, and extension.
			if b.Len()+len(piece) > 160 {
				break
			}
			b.WriteString(piece)
			lastSeparator = false
		} else if (r == '-' || r == '_') && b.Len() > 0 {
			// Preserve meaningful separators in catalogue codes, but collapse
			// repeated punctuation/spacing to one separator.
			if !lastSeparator {
				if b.Len()+1 > 160 {
					break
				}
				b.WriteByte(r)
				lastSeparator = true
			}
		} else if b.Len() > 0 && !lastSeparator {
			if b.Len()+1 > 160 {
				break
			}
			b.WriteByte('-')
			lastSeparator = true
		}
	}
	name := strings.Trim(b.String(), "-_")
	if name == "" {
		return "video"
	}
	return name
}

func copyOriginalToArchive(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()

	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(destination)
		return closeErr
	}
	return nil
}

// moveFileToPath prefers an atomic rename and falls back to a copy when the
// upload/staging directories are on different filesystems.
func moveFileToPath(source, destination string) error {
	if filepath.Clean(source) == filepath.Clean(destination) {
		return nil
	}
	if err := os.Rename(source, destination); err == nil {
		return nil
	}

	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(destination)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(destination)
		return closeErr
	}
	if err := os.Remove(source); err != nil {
		return err
	}
	return nil
}

// fitQualityDimensions returns an aspect-ratio-preserving output size constrained
// by a profile's maximum width/height. It refuses to upscale sources. Both
// dimensions are rounded down to even values for common H.264/HLS compatibility.
func fitQualityDimensions(sourceWidth, sourceHeight, maxWidth, maxHeight int64) (int64, int64, bool) {
	if sourceWidth < 2 || sourceHeight < 2 || maxWidth < 2 || maxHeight < 2 {
		return 0, 0, false
	}
	scaleW := float64(maxWidth) / float64(sourceWidth)
	scaleH := float64(maxHeight) / float64(sourceHeight)
	scale := math.Min(scaleW, scaleH)
	if scale > 1.0 {
		return 0, 0, false
	}
	width := int64(math.Floor(float64(sourceWidth)*scale/2.0)) * 2
	height := int64(math.Floor(float64(sourceHeight)*scale/2.0)) * 2
	if width < 2 || height < 2 || width > maxWidth || height > maxHeight || width > sourceWidth || height > sourceHeight {
		return 0, 0, false
	}
	return width, height, true
}

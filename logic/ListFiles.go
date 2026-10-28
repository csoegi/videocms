package logic

import (
	"ch/kirari04/videocms/models"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// LinkListItem is the row shape returned by the file list and search
// endpoints. It carries enough metadata to render a library row without
// a follow-up per-file request.
type LinkListItem struct {
	ID             uint
	CreatedAt      *time.Time
	UpdatedAt      *time.Time
	UUID           string
	Name           string
	ParentFolderID uint
	Tags           []*models.Tag
	Size           int64
	Duration       float64
	Thumbnail      string
	Processing     bool
	// PublicationStatus is the file-level publication state used by the
	// library UI. Values are: ready, processing, failed, or unknown.
	PublicationStatus string
	VideoTypes        []string
}

func (s *Service) buildLinkListItems(links []models.Link) *[]LinkListItem {
	tasksByFileID := s.publisherTasksByFileID(links)
	items := make([]LinkListItem, 0, len(links))
	for _, link := range links {
		processing := false
		videoTypes := make([]string, 0, len(link.File.Qualitys))
		seenTypes := make(map[string]struct{}, len(link.File.Qualitys))
		for _, quality := range link.File.Qualitys {
			// Converter claims keep Ready=true while Encoding=true; treat either
			// state as processing so the library reflects the active conversion.
			if quality.Encoding || (!quality.Ready && !quality.Failed) {
				processing = true
			}
			kind := strings.ToUpper(strings.TrimSpace(quality.Type))
			if kind != "" {
				if _, exists := seenTypes[kind]; !exists {
					seenTypes[kind] = struct{}{}
					videoTypes = append(videoTypes, kind)
				}
			}
		}
		fileID := link.File.ID
		if fileID == 0 {
			fileID = link.FileID
		}
		task, hasTask := tasksByFileID[fileID]
		items = append(items, LinkListItem{
			ID:                link.ID,
			CreatedAt:         link.CreatedAt,
			UpdatedAt:         link.UpdatedAt,
			UUID:              link.UUID,
			Name:              link.Name,
			ParentFolderID:    link.ParentFolderID,
			Tags:              link.Tags,
			Size:              link.File.Size,
			Duration:          link.File.Duration,
			Thumbnail:         s.ResolvedThumbnailURL(link),
			Processing:        processing,
			PublicationStatus: derivePublicationStatus(link.File, task.Status, hasTask),
			VideoTypes:        videoTypes,
		})
	}
	return &items
}

// publisherTasksByFileID loads task states in one query for the whole list,
// avoiding an N+1 query pattern for large libraries.
func (s *Service) publisherTasksByFileID(links []models.Link) map[uint]models.PublisherTask {
	tasksByFileID := make(map[uint]models.PublisherTask)
	fileIDs := make([]uint, 0, len(links))
	seen := make(map[uint]struct{}, len(links))
	for _, link := range links {
		fileID := link.File.ID
		if fileID == 0 {
			fileID = link.FileID
		}
		if fileID == 0 {
			continue
		}
		if _, exists := seen[fileID]; exists {
			continue
		}
		seen[fileID] = struct{}{}
		fileIDs = append(fileIDs, fileID)
	}
	if len(fileIDs) == 0 {
		return tasksByFileID
	}
	var tasks []models.PublisherTask
	if err := s.Deps.DB.Where("file_id IN ?", fileIDs).Find(&tasks).Error; err != nil {
		log.Printf("Failed to load publisher tasks for library list: %v", err)
		return tasksByFileID
	}
	for _, task := range tasks {
		tasksByFileID[task.FileID] = task
	}
	return tasksByFileID
}

// derivePublicationStatus uses the durable publisher task as the authority
// when one exists. It falls back to asset states for older videos created
// before PublisherTask was introduced.
func derivePublicationStatus(file models.File, taskStatus string, hasTask bool) string {
	qualities := file.Qualitys
	audios := file.Audios
	subtitles := file.Subtitles
	assetCount := len(qualities) + len(audios) + len(subtitles)

	anyEncoding := false
	anyPublishing := false
	anyFailed := false
	allAssetsReady := true
	for _, item := range qualities {
		anyEncoding = anyEncoding || item.Encoding
		anyPublishing = anyPublishing || item.Publishing
		anyFailed = anyFailed || item.Failed
		allAssetsReady = allAssetsReady && item.Ready
	}
	for _, item := range audios {
		anyEncoding = anyEncoding || item.Encoding
		anyPublishing = anyPublishing || item.Publishing
		anyFailed = anyFailed || item.Failed
		allAssetsReady = allAssetsReady && item.Ready
	}
	for _, item := range subtitles {
		anyEncoding = anyEncoding || item.Encoding
		anyPublishing = anyPublishing || item.Publishing
		anyFailed = anyFailed || item.Failed
		allAssetsReady = allAssetsReady && item.Ready
	}

	// Re-encoding/re-publishing supersedes a previous ready or failed state.
	if anyEncoding {
		return "processing"
	}
	if anyFailed {
		return "failed"
	}
	if hasTask {
		switch taskStatus {
		case "pending", "running":
			return "processing"
		case "failed":
			return "failed"
		case "ready":
			if anyPublishing {
				return "processing"
			}
			return "ready"
		}
	}

	// Legacy records may have no publisher_tasks row. Derive their state from
	// all known assets so existing published videos still show as Ready.
	if anyPublishing || (assetCount > 0 && !allAssetsReady) {
		return "processing"
	}
	if len(qualities) > 0 && allAssetsReady {
		return "ready"
	}
	return "unknown"
}

func (s *Service) ListFiles(fromFolder uint, userId uint) (status int, response *[]LinkListItem, err error) {
	//check if requested folder exists
	if fromFolder > 0 {
		res := s.Deps.DB.First(&models.Folder{}, fromFolder)
		if res.Error != nil {
			return http.StatusBadRequest, nil, errors.New("parent folder doesn't exist")
		}
	}

	// query all files
	var links []models.Link
	res := s.Deps.DB.
		Model(&models.Link{}).
		Preload("User").
		Preload("File").
		Preload("File.Qualitys").
		Preload("File.Audios").
		Preload("File.Subtitles").
		Preload("Tags").
		Where(&models.Link{
			ParentFolderID: fromFolder,
			UserID:         userId,
		}, "ParentFolderID", "UserID").
		Order("name ASC").
		Find(&links)
	if res.Error != nil {
		log.Printf("Failed to query file list: %v", res.Error)
		return http.StatusInternalServerError, nil, echo.ErrInternalServerError
	}

	return http.StatusOK, s.buildLinkListItems(links), nil
}

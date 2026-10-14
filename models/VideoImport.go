package models

// VideoImport stores importer idempotency state separately from the legacy media
// tables. This is intentionally additive: older File/Link schemas are unchanged.
type VideoImport struct {
	Model
	UserID         uint   `gorm:"not null;uniqueIndex:idx_video_import_user_request,priority:1;index"`
	RequestID      string `gorm:"size:36;not null;uniqueIndex:idx_video_import_user_request,priority:2"`
	ImportKey      string `gorm:"size:36;not null;uniqueIndex"`
	SourceEmbedURL string `gorm:"size:255"`
	SourceEmbedKey string `gorm:"size:160;index"`
	FileUUID       string `gorm:"size:36;not null;uniqueIndex"`
	LinkUUID       string `gorm:"size:36;not null;uniqueIndex"`
	Status         string `gorm:"size:20;not null;default:'pending';index"`
}

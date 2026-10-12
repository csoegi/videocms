package models

import "time"

// PublisherTask is a durable, file-level publication queue entry. A task is
// unique per file so retries cannot create duplicate publication jobs.
type PublisherTask struct {
	Model
	FileID      uint   `gorm:"uniqueIndex;not null"`
	FileUUID    string `gorm:"size:120;not null;index"`
	Status      string `gorm:"size:20;not null;default:'pending';index"` // pending, running, failed, ready
	Attempts    int
	LastError   string `gorm:"type:text"`
	StartedAt   *time.Time
	CompletedAt *time.Time
}

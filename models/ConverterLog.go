package models

import "time"

type ConverterLog struct {
	Model
	FileID uint
	QualityID uint
	InputPath string
	OutputPath string
	CycleKey string
	Attempt int
	Status string
	Error string
	PlaylistValid bool
	SegmentsValid bool
	StartedAt time.Time
	FinishedAt *time.Time
	DurationMs int64
}

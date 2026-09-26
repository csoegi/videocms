package models

import "gorm.io/gorm"

type Subtitle struct {
	Model
	UUID          string
	Name          string `gorm:"size:120;"`
	Lang          string `gorm:"size:10;"`
	Path          string `gorm:"size:120;" json:"path"`
	OriginalCodec string `json:"-"`
	Index         int    `json:"-"`
	Codec         string
	Type          string 
	OutputFile    string
	Encoding      bool
	Progress      float64
	Failed        bool
	Ready         bool
	Error         string `json:"-"`
	File          File   `json:"-"`
	FileID        uint

	// --- Soft/Hard burning subtitles
	Source          string `gorm:"type:varchar(20);default:'internal'" json:"source"` // "internal" | "external"
	SelectedForBurn bool `gorm:"default:false" json:"selected_for_burn"` // True for hard-burned
}

// Subtitle tracking data serialized inside the UploadSession storage block.
type SubtitleSessionData struct {
	UUID            string `json:"uuid"`
	Name            string `json:"name"`
	Lang            string `json:"lang"`
	Path            string `json:"path"`
	SelectedForBurn bool   `json:"selected_for_burn"`
}

type SubtitleSettingValidation struct {
	SubtitleMode        string `json:"SubtitleMode" validate:"omitempty,oneof=none soft hard"`
	SelectedBurnUUID  	string `json:"SelectedBurnUUID" validate:"omitempty"`
}


func (c *Subtitle) SetProcess(v float64) {
	c.Progress = v
}
func (c *Subtitle) GetProcess() float64 {
	return c.Progress
}
func (c *Subtitle) Save(DB *gorm.DB) *gorm.DB {
	return DB.Save(c)
}

type AvailableSubtitle struct {
	Type       string // ass | vtt
	Codec      string
	OutputFile string
}

var AvailableSubtitles = []AvailableSubtitle{
	{
		Type:       "ass",
		Codec:      "ass",
		OutputFile: "out.ass",
	},
	{
		Type:       "vtt",
		Codec:      "webvtt",
		OutputFile: "out.vtt",
	},
}

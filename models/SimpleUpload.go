package models

type SimpleUploadValidation struct {
	Name           string `validate:"required,min=1,max=128" form:"Name"`
	ParentFolderID uint   `validate:"number" form:"ParentFolderID"`

	// --- Sudt/Hard burning subtitle
	SubtitleMode        string `form:"subtitle_mode" validate:"omitempty,oneof=none soft hard"`
	SelectedSubForBurn  string `form:"selected_sub_for_burn" validate:"omitempty"`
}

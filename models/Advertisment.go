package models

type Advertisement struct {
	Model
	User        User `json:"-"`
	UserID      uint
	AdsName     string `gorm:"type:varchar(255);not null"`
	AdsType     string `gorm:"type:varchar(50);not null;index"`      // video | banner [Use index for faster queries when filtering by ad type]
	AdsLink     string `gorm:"type:text;not null"`                   // Path to mp4 or static page slug (/p/my-ads)
	AdsDisplay  string `gorm:"type:varchar(50);not null;index"`      // before_start | mid_roll [Use index for faster queries when filtering by display position]
	AdsDevice   string `gorm:"type:varchar(50);default:'all';index"` // all | desktop | mobile | tablet [Use index for faster queries when filtering by device]
	AdsPriority int    `gorm:"type:int;default:0;index"`             // Higher number = higher priority [Use index for faster sorting by priority]
	IsActive    bool   `gorm:"type:boolean;default:true;index"`		 // Whether the ad is active [Use index for faster filtering by active status]
}

type AdvertisementCreateValidation struct {
	AdsName     string `json:"ads_name" validate:"required,min=3,max=255"`
	AdsType     string `json:"ads_type" validate:"required,oneof=video banner"`
	AdsLink     string `json:"ads_link" validate:"required"`
	AdsDisplay  string `json:"ads_display" validate:"required"`
	AdsDevice   string `json:"ads_device" validate:"required,oneof=all desktop mobile tablet"`
	AdsPriority int    `json:"ads_priority" validate:"min=0"`
	IsActive    bool   `json:"is_active"`
}

type AdvertisementUpdateValidation struct {
	AdsName     string `json:"ads_name" validate:"omitempty,min=3,max=255"`
	AdsType     string `json:"ads_type" validate:"omitempty,oneof=video banner"`
	AdsLink     string `json:"ads_link" validate:"omitempty"`
	AdsDisplay  string `json:"ads_display" validate:"omitempty"`
	AdsDevice   string `json:"ads_device" validate:"omitempty,oneof=all desktop mobile tablet"`
	AdsPriority *int   `json:"ads_priority" validate:"omitempty,min=0"`
	IsActive    *bool  `json:"is_active"`
}
package inits

import (
	"ch/kirari04/videocms/models"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type legacyWebPage struct {
	models.Model
	Path         string
	Title        string
	HTML         string `gorm:"column:html"`
	ListInFooter bool
}

func (legacyWebPage) TableName() string {
	return "web_pages"
}

func TestMigrateModelsPreservesLegacyWebPages(t *testing.T) {
	db, err := gorm.Open(
		sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"),
		&gorm.Config{},
	)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.AutoMigrate(&legacyWebPage{}); err != nil {
		t.Fatalf("migrate legacy webpage: %v", err)
	}
	if err := db.Create(&legacyWebPage{
		Path:         "/privacy/",
		Title:        "Privacy",
		HTML:         "<p>Legacy content</p>",
		ListInFooter: true,
	}).Error; err != nil {
		t.Fatalf("create legacy webpage: %v", err)
	}
	if err := db.Exec(`
		CREATE TABLE traffic_logs (
			id integer PRIMARY KEY AUTOINCREMENT,
			created_at datetime,
			updated_at datetime,
			deleted_at datetime,
			user_id integer,
			file_id integer,
			quality_id integer,
			audio_id integer,
			bytes integer
		)
	`).Error; err != nil {
		t.Fatalf("create legacy traffic table: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO traffic_logs (created_at, user_id, file_id, quality_id, audio_id, bytes)
		VALUES (CURRENT_TIMESTAMP, 1, 2, 3, 4, 512)
	`).Error; err != nil {
		t.Fatalf("create legacy traffic row: %v", err)
	}
	if err := MigrateModels(db); err != nil {
		t.Fatalf("MigrateModels() error = %v", err)
	}

	// Import support is additive: it creates its own table and does not require
	// adding idempotency columns to legacy media tables.
	if !db.Migrator().HasTable(&models.VideoImport{}) {
		t.Fatal("expected additive video_imports table to be created")
	}
	if db.Migrator().HasColumn(&models.File{}, "ImportKey") {
		t.Fatal("File schema must not gain an ImportKey column")
	}
	firstImport := models.VideoImport{
		UserID: 7, RequestID: "b9dbdd50-6d4e-4d25-a96d-7dcb6c93e17b",
		ImportKey: "e86cb31a-a46f-4f15-9a1e-ae45d3f44222", FileUUID: "7b90cc6e-7d4b-4c8e-a0c8-9d5f4c04d77e",
		LinkUUID: "9c24efaa-bb08-41d4-83c9-59b83f8e2c2d", Status: "pending",
	}
	if err := db.Create(&firstImport).Error; err != nil {
		t.Fatalf("create video import: %v", err)
	}
	duplicateRequest := firstImport
	duplicateRequest.ID = 0
	duplicateRequest.ImportKey = "0ec6e019-f94d-4d36-a0c9-b3adac54f1f1"
	if err := db.Create(&duplicateRequest).Error; err == nil {
		t.Fatal("expected duplicate (user_id, request_id) to be rejected")
	}

	var page models.WebPage
	if err := db.First(&page).Error; err != nil {
		t.Fatalf("load migrated webpage: %v", err)
	}
	if page.Content != "<p>Legacy content</p>" {
		t.Fatalf("content = %q, want legacy HTML", page.Content)
	}
	if page.Format != models.WebPageFormatHTML {
		t.Fatalf("format = %q, want %q", page.Format, models.WebPageFormatHTML)
	}
	if !page.Published {
		t.Fatal("expected legacy webpage to remain published")
	}

	var traffic models.TrafficLog
	if err := db.First(&traffic).Error; err != nil {
		t.Fatalf("load migrated traffic: %v", err)
	}
	if traffic.Source != models.TrafficSourcePlayer || traffic.Bytes != 512 {
		t.Fatalf("migrated traffic = %#v, want player source with 512 bytes", traffic)
	}
}

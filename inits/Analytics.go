package inits

import (
	"log"
	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	"github.com/oschwald/maxminddb-golang"
)

var AnalyticsDB *badger.DB
var GeoIP *maxminddb.Reader

func SetupAnalytics() error {
	var err error
	opts := badger.DefaultOptions("./database/analytics")
	
	// Performance & Size Optimizations
	opts.ValueLogFileSize = 16 * 1024 * 1024 // 16MB files are easier to GC
	opts.IndexCacheSize = 10 * 1024 * 1024
	opts.Compression = options.ZSTD         // Huge space saver for JSON
	opts.NumVersionsToKeep = 1              // Strictly keep only current data
	opts.CompactL0OnClose = true            // Clean up on exit
	opts.Logger = nil                       // Silence internal verbose logs

	// CRITICAL FIX: Return the error to InitRuntime instead of forcing log.Fatal
	AnalyticsDB, err = badger.Open(opts)
	if err != nil {
		return err 
	}

	GeoIP, err = maxminddb.Open("./data/geo-ip/GeoLite2-Country.mmdb")
	if err != nil {
		// Log warning but don't stop execution so the app can run without GeoIP functionality
		log.Printf("⚠️ GeoIP disabled: %v", err)
	}

	log.Println("📊 Analytics Engine Initialized")
	return nil
}

package logic

import (
	"ch/kirari04/videocms/helpers" // Fix 1: Used to access GetDeviceCategory
	"ch/kirari04/videocms/inits"
	"ch/kirari04/videocms/models"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"sort" // Fix 4: Added missing sort library
	"time"

	"github.com/dgraph-io/badger/v4"
)

// Internal structs kept inside package logic to process BadgerDB streams cleanly
type EventLog struct {
	Event     string `json:"e"`
	VideoID   string `json:"v"`
	Country   string `json:"c"`
	Device    string `json:"d"`
	IP        string `json:"ip"` 
	UA        string `json:"ua"` 
	Timestamp int64  `json:"t"`
}

type AnalyticsReport struct {
	TotalViews     uint64      `json:"total_views"`
	TotalStreams   uint64      `json:"total_streams"`
	TotalDownloads uint64      `json:"total_downloads"`
	DailyStats     []DailyStat `json:"daily_stats"`
	TopViewed      []TopItem   `json:"top_viewed"`    
	TopStreamed    []TopItem   `json:"top_streamed"`  
	TopDownloaded  []TopItem   `json:"top_downloaded"`
	TopCountries   []TopItem   `json:"top_countries"`
	TopDevices     []TopItem   `json:"top_devices"`
	LastUpdated    time.Time   `json:"last_updated"`
}

type DailyStat struct {
	Date      string `json:"date"`
	Views     uint64 `json:"views"`
	Streams   uint64 `json:"streams"`
	Downloads uint64 `json:"downloads"`
}

type TopItem struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// CachedReport stores the computed layout values for the dashboard endpoints
var CachedReport *AnalyticsReport 

func (s *Service) RecordEvent(eventType string, videoID string, ip string, ua string) {
	cfg := s.Config()
	if !cfg.AnalyticsEnabled {
		return
	}

	now := time.Now()
	dateKey := now.Format("2006-01-02")
	
	// Country Lookup
	country := "Unknown"
	if inits.GeoIP != nil {
		var record struct {
			Country struct {
				IsoCode string `maxminddb:"iso_code"`
			} `maxminddb:"country"`
		}
		parsedIP := net.ParseIP(ip)
		if parsedIP != nil {
			if err := inits.GeoIP.Lookup(parsedIP, &record); err == nil && record.Country.IsoCode != "" {
				country = record.Country.IsoCode
			}
		}
	}

	// Fix 1: Properly scope the helper call to find your device parser
	device := helpers.GetDeviceCategory(ua) 

	// Batch Write to Badger
	err := inits.AnalyticsDB.Update(func(txn *badger.Txn) error {
		countKey := []byte(fmt.Sprintf("stat:%s:%s", dateKey, eventType))
		s.incrementCounter(txn, countKey)

		// Fix 2: Instantiate using local EventLog struct type definition matching the read scan
		event := EventLog{
			Event:     eventType,
			VideoID:   videoID,
			Country:   country,
			Device:    device,
			IP:        ip,
			UA:        ua,
			Timestamp: now.Unix(),
		}
		
		eventJSON, _ := json.Marshal(event)
		eventKey := []byte(fmt.Sprintf("log:%d:%s", now.UnixNano(), videoID))
		
		ttl := time.Duration(cfg.AnalyticsLogDuration) * 24 * time.Hour

		entry := badger.NewEntry(eventKey, eventJSON).WithTTL(ttl)
		return txn.SetEntry(entry)
	})

	if err != nil {
		fmt.Printf("Analytics Error: %v\n", err)
	}
}

func (s *Service) BuildAnalyticsReport() {
	// Fix 3: Instantiate local AnalyticsReport instead of conflicting models structure
	newReport := &AnalyticsReport{
		LastUpdated: time.Now(),
	}

	var links []models.Link
	videoTitleMap := make(map[string]string)
	s.Deps.DB.Select("uuid, name").Find(&links)
	for _, l := range links {
		videoTitleMap[l.UUID] = l.Name
	}

	viewCounts := make(map[string]int)
	streamCounts := make(map[string]int)
	downloadCounts := make(map[string]int)
	countryCounts := make(map[string]int)
	deviceCounts := make(map[string]int)
	thirtyDaysAgo := time.Now().AddDate(0, 0, -30).Unix()

	inits.AnalyticsDB.View(func(txn *badger.Txn) error {
		for i := 0; i < 30; i++ {
			d := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
			// Fix 3: Appends matching local DailyStat slice element type perfectly
			newReport.DailyStats = append(newReport.DailyStats, DailyStat{
				Date:      d,
				Views:     getCounterValue(txn, fmt.Sprintf("stat:%s:view", d)),
				Streams:   getCounterValue(txn, fmt.Sprintf("stat:%s:stream", d)),
				Downloads: getCounterValue(txn, fmt.Sprintf("stat:%s:download", d)),
			})
		}

		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		prefix := []byte("log:")
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			var logData EventLog
			_ = it.Item().Value(func(v []byte) error { return json.Unmarshal(v, &logData) })

			if logData.Timestamp < thirtyDaysAgo {
				continue
			}

			switch logData.Event {
			case "view":
				viewCounts[logData.VideoID]++
				newReport.TotalViews++
			case "stream":
				streamCounts[logData.VideoID]++
				newReport.TotalStreams++
			case "download":
				downloadCounts[logData.VideoID]++
				newReport.TotalDownloads++
			}
			countryCounts[logData.Country]++
			deviceCounts[logData.Device]++
		}
		return nil
	})

	// Fix 3: All target mappings now process uniform local struct objects beautifully
	newReport.TopViewed = sortMapToTopItems(viewCounts, videoTitleMap, 10)
	newReport.TopStreamed = sortMapToTopItems(streamCounts, videoTitleMap, 10)
	newReport.TopDownloaded = sortMapToTopItems(downloadCounts, videoTitleMap, 10)
	newReport.TopCountries = sortMapToTopItems(countryCounts, nil, 5)
	newReport.TopDevices = sortMapToTopItems(deviceCounts, nil, 5)

	CachedReport = newReport
}

func (s *Service) incrementCounter(txn *badger.Txn, key []byte) {
	var val uint64
	item, err := txn.Get(key)
	if err == nil {
		_ = item.Value(func(v []byte) error {
			val = binary.LittleEndian.Uint64(v)
			return nil
		})
	}
	val++
	newVal := make([]byte, 8)
	binary.LittleEndian.PutUint64(newVal, val)
	_ = txn.Set(key, newVal)
}

func getCounterValue(txn *badger.Txn, key string) uint64 {
	item, err := txn.Get([]byte(key))
	if err != nil {
		return 0
	}
	var val uint64
	_ = item.Value(func(v []byte) error {
		val = binary.LittleEndian.Uint64(v)
		return nil
	})
	return val
}

func sortMapToTopItems(m map[string]int, labels map[string]string, limit int) []TopItem {
	type kv struct {
		Key   string
		Value int
	}
	var ss []kv
	for k, v := range m {
		ss = append(ss, kv{k, v})
	}
	sort.Slice(ss, func(i, j int) bool {
		return ss[i].Value > ss[j].Value
	})

	res := []TopItem{}
	for i, item := range ss {
		if i >= limit {
			break
		}
		label := item.Key
		if labels != nil && labels[item.Key] != "" {
			label = labels[item.Key]
		}
		res = append(res, TopItem{Label: label, Count: item.Value})
	}
	return res
}

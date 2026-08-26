package services

import (
	"ch/kirari04/videocms/inits"
	"context"
	"log"
	"time"
)

func (w *WorkerGroup) AnalyticsWorker(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	// 1. Initial configuration check on boot
	if !w.Config().AnalyticsEnabled {
		log.Println("📊 Analytics worker is disabled.")
		return
	}

	// 2. Initial build on startup using your actual helper
	log.Println("📊 Analytics: Generating initial report...")
	w.logic.BuildAnalyticsReport() // WorkerGroup use w.logic (lowercase!)

	for {
		// Fetch interval values dynamically from the configuration store
		interval := w.Config().AnalyticsWorkerInterval
		if interval <= 0 {
			interval = 60 // Safe fallback
		}

		// Cleanly block until next interval or context cancel termination
		if !sleepContext(ctx, time.Duration(interval)*time.Minute) {
			return
		}

		// Re-evaluate toggle conditions at runtime
		if w.Config().AnalyticsEnabled {
			log.Println("🔄 Analytics: Updating report cache...")
			
			// Execute your real processing handler here
			w.logic.BuildAnalyticsReport()
		}

		// Reclaim disk space by cleaning up expired TTL value logs in BadgerDB
		log.Println("🧹 Analytics: Running BadgerDB Garbage Collection...")
		for {
			if err := inits.AnalyticsDB.RunValueLogGC(0.5); err != nil {
				break // Exit loop when no more log structures can be optimized
			}
		}
	}
}

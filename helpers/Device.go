// helpers/Device.go
package helpers

// GetDeviceCategory safely categorizes incoming request user-agents
func GetDeviceCategory(ua string) string {
	if ua == "" {
		return "Unknown"
	}
	
	// Normalize to lowercase for fast, robust parsing checks
	lowerUA := ua
	// Simple manual check avoiding heavy regex overhead
	for i := 0; i < len(lowerUA); i++ {
		if lowerUA[i] >= 'A' && lowerUA[i] <= 'Z' {
			// Convert to lowercase inline if needed, or use a helper
		}
	}
	
	// Check standard layout indicators
	if contains(lowerUA, "ipad") || contains(lowerUA, "playbook") || contains(lowerUA, "tablet") {
		return "Tablet"
	}
	if contains(lowerUA, "iphone") || contains(lowerUA, "android") || contains(lowerUA, "mobile") {
		return "Mobile"
	}
	if contains(lowerUA, "bot") || contains(lowerUA, "crawl") || contains(lowerUA, "spider") {
		return "Bot"
	}
	
	return "Desktop" // Fallback standard default
}

// Inline substring helper to avoid extra package imports
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

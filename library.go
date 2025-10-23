package mc_router

import (
	"strings"
)

// SplitExternalHosts splits a string containing external hostnames by comma and/or newline delimiters.
// It trims whitespace around each hostname and filters out empty strings.
// Examples:
//   - "host1.com,host2.com" -> ["host1.com", "host2.com"]
//   - "host1.com, host2.com" -> ["host1.com", "host2.com"]
//   - "host1.com\nhost2.com" -> ["host1.com", "host2.com"]
//   - "host1.com,\nhost2.com" -> ["host1.com", "host2.com"]
func SplitExternalHosts(s string) []string {
	// First, replace newlines with commas to normalize the delimiter
	normalized := strings.ReplaceAll(s, "\n", ",")
	
	// Split by comma
	parts := strings.Split(normalized, ",")
	
	// Trim whitespace and filter out empty strings
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	
	return result
}

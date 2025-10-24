package server

import (
	"regexp"
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
	// Use regexp to split on either comma or newline
	re := regexp.MustCompile(",|\n")
	parts := re.Split(s, -1)

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

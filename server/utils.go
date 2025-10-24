package server

import (
	"regexp"
	"strings"
)

// splitPattern is the regex pattern used to split external host definitions.
// kept as a const so the literal is easy to see and reuse in tests or docs.
const splitPattern = ",|\n"

// splitRe is the compiled regexp for splitPattern. Compiling once at package
// initialization avoids repeated compilation on every call to
// SplitExternalHosts and is slightly more efficient.
var splitRe = regexp.MustCompile(splitPattern)

// SplitExternalHosts splits a string containing external hostnames by comma and/or newline delimiters.
// It trims whitespace around each hostname and filters out empty strings.
// Examples:
//   - "host1.com,host2.com" -> ["host1.com", "host2.com"]
//   - "host1.com, host2.com" -> ["host1.com", "host2.com"]
//   - "host1.com\nhost2.com" -> ["host1.com", "host2.com"]
//   - "host1.com,\nhost2.com" -> ["host1.com", "host2.com"]
func SplitExternalHosts(s string) []string {
	// Use regexp to split on either comma or newline
	parts := splitRe.Split(s, -1)

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

// Package discoveryconfig parses configuration values shared by discovery sources.
package discoveryconfig

import (
	"strconv"
	"strings"
)

// ParseBool trims surrounding whitespace and accepts the boolean spellings
// supported by strconv.ParseBool. Callers decide how absent or invalid settings
// affect discovery and provide the relevant label or annotation logging context.
func ParseBool(value string) (bool, error) {
	return strconv.ParseBool(strings.TrimSpace(value))
}

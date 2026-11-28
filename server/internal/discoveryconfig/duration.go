package discoveryconfig

import (
	"fmt"
	"strings"
	"time"
)

// ParsePositiveDuration parses a discovery override. Absence and invalid-value
// fallback are handled by the provider so warnings include resource context.
func ParsePositiveDuration(value string) (time.Duration, error) {
	duration, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid duration: %w", err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return duration, nil
}

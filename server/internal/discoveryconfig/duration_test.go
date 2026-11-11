package discoveryconfig

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePositiveDuration(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  time.Duration
	}{
		{"12h", 12 * time.Hour}, {" 30s ", 30 * time.Second}, {"1.5m", 90 * time.Second},
		{"", 0}, {"0", 0}, {"0s", 0}, {"-1m", 0}, {"12", 0}, {"forever", 0}, {"999999999999999999h", 0},
	} {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParsePositiveDuration(tt.input)
			if tt.want == 0 {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

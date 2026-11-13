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
		{
			input: "12h",
			want:  12 * time.Hour,
		},
		{
			input: " 30s ",
			want:  30 * time.Second,
		},
		{
			input: "1.5m",
			want:  90 * time.Second,
		},
		{
			input: "",
			want:  0,
		},
		{
			input: "0",
			want:  0,
		},
		{
			input: "0s",
			want:  0,
		},
		{
			input: "-1m",
			want:  0,
		},
		{
			input: "12",
			want:  0,
		},
		{
			input: "forever",
			want:  0,
		},
		{
			input: "999999999999999999h",
			want:  0,
		},
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

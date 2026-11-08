package discoveryconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBool(t *testing.T) {
	for _, test := range []struct {
		name    string
		inputs  []string
		want    bool
		invalid bool
	}{
		{
			name:   "true spellings",
			inputs: []string{"true", "TRUE", "True", "t", "T", "1"},
			want:   true,
		},
		{
			name:   "false spellings",
			inputs: []string{"false", "FALSE", "False", "f", "F", "0"},
		},
		{
			name:   "whitespace around true",
			inputs: []string{" true ", "\tTRUE\n"},
			want:   true,
		},
		{
			name:   "whitespace around false",
			inputs: []string{" false ", "\tFalse\n"},
		},
		{
			name:    "invalid values remain errors",
			inputs:  []string{"", " \t\n", "yes", "no", "enabled", "TrUe", "2"},
			invalid: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, input := range test.inputs {
				got, err := ParseBool(input)
				if test.invalid {
					require.Error(t, err, "input %q", input)
					continue
				}
				require.NoError(t, err, "input %q", input)
				assert.Equal(t, test.want, got, "input %q", input)
			}
		})
	}
}

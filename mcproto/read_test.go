package mcproto

import (
	"bytes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestReadVarInt(t *testing.T) {
	tests := []struct {
		Name     string
		Input    []byte
		Expected int
	}{
		{
			Name:     "Single byte",
			Input:    []byte{0xFA, 0x00},
			Expected: 0x7A,
		},
		{
			Name:     "Two byte",
			Input:    []byte{0x81, 0x04},
			Expected: 0x0201,
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			result, err := ReadVarInt(bytes.NewBuffer(tt.Input))
			require.NoError(t, err)

			assert.Equal(t, tt.Expected, result)
		})
	}
}

package mcproto

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestHandshakeThenStatus(t *testing.T) {
	content, err := ReadHexDumpFile("handshake-status.hex")
	require.NoError(t, err)

	reader := bufio.NewReader(bytes.NewReader(content))

	handshakePacket, err := ReadPacket(reader, nil, StateHandshaking)
	require.NoError(t, err)

	handshake, err := DecodeHandshake(handshakePacket.Data)
	require.NoError(t, err)

	assert.Equal(t, "localhost", handshake.ServerAddress)
	assert.Equal(t, uint16(25565), handshake.ServerPort)
	assert.Equal(t, 770 /*for 1.21.5*/, handshake.ProtocolVersion)
	assert.Equal(t, StateStatus, handshake.NextState)
}

func TestHandshakeThenLoginStart(t *testing.T) {
	content, err := ReadHexDumpFile("handshake-login-start.hex")
	require.NoError(t, err)

	reader := bufio.NewReader(bytes.NewReader(content))

	handshakePacket, err := ReadPacket(reader, nil, StateHandshaking)
	require.NoError(t, err)

	handshake, err := DecodeHandshake(handshakePacket.Data)
	require.NoError(t, err)

	assert.Equal(t, "localhost", handshake.ServerAddress)
	assert.Equal(t, uint16(25565), handshake.ServerPort)
	assert.Equal(t, 770 /*for 1.21.5*/, handshake.ProtocolVersion)
	assert.Equal(t, StateLogin, handshake.NextState)

	loginStartPacket, err := ReadPacket(reader, nil, StateLogin)
	require.NoError(t, err)

	loginStart, err := DecodeLoginStart(loginStartPacket.Data)
	require.NoError(t, err)

	assert.Equal(t, "itzg", loginStart.Name)
	assert.Equal(t, uuid.MustParse("5cddfd26-fc86-4981-b52e-c42bb10bfdef"), loginStart.PlayerUuid)
}

func ReadHexDumpFile(filename string) ([]byte, error) {
	// Read the file content
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Convert content to string and clean it up
	hexString := string(content)

	// Remove whitespace and newlines
	hexString = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1 // Remove spaces, tabs, newlines
		}
		return r
	}, hexString)

	return hex.DecodeString(hexString)
}

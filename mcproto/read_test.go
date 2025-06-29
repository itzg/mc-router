package mcproto

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/google/uuid"

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
	assert.Equal(t, ProtocolVersion1_21_5, handshake.ProtocolVersion)
	assert.Equal(t, StateStatus, handshake.NextState)
}

func TestHandshakeThenLoginStartVersion(t *testing.T) {
	playerUuid := uuid.MustParse("5cddfd26-fc86-4981-b52e-c42bb10bfdef")

	tests := []struct {
		Name                    string
		Filename                string
		ExpectedProtocolVersion ProtocolVersion
		ExpectedPlayerUuid      uuid.UUID
	}{
		{
			Name:                    "1.20.2",
			Filename:                "handshake-login-start-1.21.5.hex",
			ExpectedProtocolVersion: ProtocolVersion1_21_5,
			ExpectedPlayerUuid:      playerUuid,
		},
		// This version only conditionally provides a UUID, and may provide other information
		// as well
		{
			Name:                    "1.19.2-all-info",
			Filename:                "handshake-login-start-1.19.2-all-info.hex",
			ExpectedProtocolVersion: ProtocolVersion1_19_2,
			ExpectedPlayerUuid:      playerUuid,
		},
		{
			Name:                    "1.19.2-min-info",
			Filename:                "handshake-login-start-1.19.2-min-info.hex",
			ExpectedProtocolVersion: ProtocolVersion1_19_2,
			ExpectedPlayerUuid:      uuid.Nil, // No UUID provided in this case
		},
		// This is the last version that does not provide a UUID
		{
			Name:                    "1.18.2",
			Filename:                "handshake-login-start-1.18.2.hex",
			ExpectedProtocolVersion: ProtocolVersion1_18_2,
			ExpectedPlayerUuid:      uuid.Nil, // No UUID provided by this version
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			content, err := ReadHexDumpFile(tt.Filename)
			require.NoError(t, err)

			reader := bufio.NewReader(bytes.NewReader(content))

			handshakePacket, err := ReadPacket(reader, nil, StateHandshaking)
			require.NoError(t, err)

			handshake, err := DecodeHandshake(handshakePacket.Data)
			require.NoError(t, err)

			assert.Equal(t, "localhost", handshake.ServerAddress)
			assert.Equal(t, uint16(25565), handshake.ServerPort)
			assert.Equal(t, tt.ExpectedProtocolVersion, handshake.ProtocolVersion)
			assert.Equal(t, StateLogin, handshake.NextState)

			loginStartPacket, err := ReadPacket(reader, nil, StateLogin)
			require.NoError(t, err)

			loginStart, err := DecodeLoginStart(handshake.ProtocolVersion, loginStartPacket.Data)
			require.NoError(t, err)

			assert.Equal(t, "itzg", loginStart.Name)
			assert.Equal(t, tt.ExpectedPlayerUuid, loginStart.PlayerUuid)
		})
	}
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

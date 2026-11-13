package mcproto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteLoginDisconnect(t *testing.T) {
	var buf bytes.Buffer
	reason := `Server "test" is starting – try again`
	require.NoError(t, WriteLoginDisconnect(&buf, reason))

	frame, err := ReadFrame(bufio.NewReader(&buf), nil)
	require.NoError(t, err)
	payload := bytes.NewReader(frame.Payload)
	packetID, err := ReadVarInt(payload)
	require.NoError(t, err)
	assert.Equal(t, PacketIdLoginDisconnect, packetID)

	jsonReason, err := ReadString(payload)
	require.NoError(t, err)
	var component map[string]string
	require.NoError(t, json.Unmarshal([]byte(jsonReason), &component))
	assert.Equal(t, reason, component["text"])
	assert.Zero(t, payload.Len())
}

func TestWriteLoginPluginRequest(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteLoginPluginRequest(&buf, 300, "mc-router:wait", []byte{0x01, 0x02}))

	frame, err := ReadFrame(bufio.NewReader(&buf), nil)
	require.NoError(t, err)
	payload := bytes.NewReader(frame.Payload)
	packetID, err := ReadVarInt(payload)
	require.NoError(t, err)
	assert.Equal(t, PacketIdLoginPluginRequest, packetID)

	messageID, err := ReadVarInt(payload)
	require.NoError(t, err)
	assert.Equal(t, 300, messageID)
	channel, err := ReadString(payload)
	require.NoError(t, err)
	assert.Equal(t, "mc-router:wait", channel)
	data, err := ReadByteArray(payload, payload.Len())
	require.NoError(t, err)
	assert.Equal(t, []byte{0x01, 0x02}, data)
}

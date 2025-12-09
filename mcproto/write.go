package mcproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"unicode/utf16"
)

// WriteVarInt writes a VarInt (Minecraft format) to w
func WriteVarInt(w io.Writer, value int32) error {
	var buf [5]byte
	i := 0
	v := uint32(value)
	for {
		temp := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			temp |= 0x80
		}
		buf[i] = temp
		i++
		if v == 0 {
			break
		}
	}
	_, err := w.Write(buf[:i])
	return err
}

// WriteString writes a Minecraft length-prefixed string
func WriteString(w io.Writer, s string) error {
	if err := WriteVarInt(w, int32(len(s))); err != nil {
		return err
	}
	_, err := io.WriteString(w, s)
	return err
}

// buildPacket builds a framed packet: [length VarInt][packetId VarInt][payload]
func buildPacket(packetID int32, payload []byte) []byte {
	var b bytes.Buffer
	_ = WriteVarInt(&b, packetID)
	b.Write(payload)

	var framed bytes.Buffer
	_ = WriteVarInt(&framed, int32(b.Len()))
	framed.Write(b.Bytes())
	return framed.Bytes()
}

// StatusResponse is a minimal structure for the status JSON
type StatusResponse struct {
	Version struct {
		Name     string `json:"name"`
		Protocol int    `json:"protocol"`
	} `json:"version"`
	Players struct {
		Max    int `json:"max"`
		Online int `json:"online"`
		Sample []struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"sample,omitempty"`
	} `json:"players"`
	Description        map[string]interface{} `json:"description"`
	Favicon            string                 `json:"favicon,omitempty"`
	EnforcesSecureChat *bool                  `json:"enforcesSecureChat,omitempty"`
}

// WriteStatusJSONPacket writes a Status Response (packet 0x00) with the provided JSON string
func WriteStatusJSONPacket(w io.Writer, jsonString string) error {
	// payload is the JSON as a Minecraft string
	var payload bytes.Buffer
	if err := WriteString(&payload, jsonString); err != nil {
		return err
	}
	pkt := buildPacket(0x00, payload.Bytes())
	_, err := w.Write(pkt)
	return err
}

// WriteStatusFromStruct writes a Status Response from a struct
func WriteStatusFromStruct(w io.Writer, status StatusResponse) error {
	b, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return WriteStatusJSONPacket(w, string(b))
}

// WritePongPacket writes Pong (packet 0x01) with the same payload
func WritePongPacket(w io.Writer, payload int64) error {
	var pl bytes.Buffer
	// payload is a signed long (64-bit)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(payload))
	pl.Write(buf[:])
	pkt := buildPacket(0x01, pl.Bytes())
	_, err := w.Write(pkt)
	return err
}

// WriteLegacySLPResponse writes the 1.6-compatible legacy response packet (0xFF)
// Format: FF, [length short], UTF16BE string beginning with "\u00A7\u0031\u0000" then null-delimited fields
// fields: protocol, version, motd, online, max
func WriteLegacySLPResponse(w io.Writer, protocol int, version string, motd string, online int, max int) error {
	// Build the string with null separators
	s := "\u00A7\u0031\u0000" +
		intToString(protocol) + "\u0000" +
		version + "\u0000" +
		motd + "\u0000" +
		intToString(online) + "\u0000" +
		intToString(max)

	// Encode UTF-16BE
	runes := []rune(s)
	encoded := utf16.Encode(runes)
	var be bytes.Buffer
	for _, v := range encoded {
		var tmp [2]byte
		binary.BigEndian.PutUint16(tmp[:], v)
		be.Write(tmp[:])
	}

	bw := bufio.NewWriter(w)
	// 0xFF
	if _, err := bw.Write([]byte{0xFF}); err != nil {
		return err
	}
	// length short in code units
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(encoded)))
	if _, err := bw.Write(lenBuf[:]); err != nil {
		return err
	}
	if _, err := bw.Write(be.Bytes()); err != nil {
		return err
	}
	return bw.Flush()
}

// helpers
func intToString(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + (i % 10))
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

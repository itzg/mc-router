package mcproto

import (
	"encoding/binary"
	"io"
	"unicode/utf16"
)

func VersionToProtocolVersion(versionClient string) int {
	switch versionClient {
	// b1.4_01 and prior require additional troubleshooting
	case "b1.5", "b1.5_01":
		return 11
	case "b1.6", "b1.6.1", "b1.6.2", "b1.6.3", "b1.6.4", "b1.6.5", "b1.6.6":
		return 13
	case "b1.7", "b1.7.2", "b1.7.3":
		return 14
	case "b1.8.1", "b1.8":
		return 17
	case "1.0":
		return 22
	case "1.1":
		return 23
	case "1.2.1", "1.2.2", "1.2.3":
		return 28
	case "1.2.4", "1.2.5":
		return 29
	// 1.3.1 onwards require additional troubleshooting
	default:
		return -1
	}
}

// readBetaString reads a legacy MC string: [Short Length] [UTF-16BE Chars...]
func ReadBetaString(r io.Reader) (string, []byte, error) {
	// Read Length (Short)
	var length int16
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return "", nil, err
	}

	// Read Characters (Length * 2 bytes)
	byteData := make([]byte, length*2)
	if _, err := io.ReadFull(r, byteData); err != nil {
		return "", nil, err
	}

	// Decode UTF-16BE to Go String
	shorts := make([]uint16, length)
	for i := 0; i < int(length); i++ {
		shorts[i] = binary.BigEndian.Uint16(byteData[i*2 : i*2+2])
	}

	// Return both the string (for logic) and raw bytes (for replaying)
	return string(utf16.Decode(shorts)), byteData, nil
}

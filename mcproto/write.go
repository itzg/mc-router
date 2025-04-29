package mcproto

import (
	"encoding/json"
	"io"
)

func WriteStatusResponse(w io.Writer, motd string) error {
    resp := StatusResponse{
        Version: StatusVersion{
            Name:     "1.21.5",
            Protocol: 770,
        },
        Players: StatusPlayers{
            Max:    0,
            Online: 0,
        },
        Description: StatusText{
            Text: motd,
        },
    }
    data, err := json.Marshal(resp)
    if err != nil {
        return err
    }

    jsonLen := encodeVarInt(len(data))
    payload := append(jsonLen, data...)
    return WritePacket(w, 0x00, payload)
}

func WritePacket(w io.Writer, packetID int, data []byte) error {
    packet := append(encodeVarInt(packetID), data...)
    length := encodeVarInt(len(packet))
    _, err := w.Write(append(length, packet...))
    return err
}

// encodeVarInt encodes an int as a Minecraft VarInt.
func encodeVarInt(value int) []byte {
    var buf []byte
    for {
        temp := byte(value & 0x7F)
        value >>= 7
        if value != 0 {
            temp |= 0x80
        }
        buf = append(buf, temp)
        if value == 0 {
            break
        }
    }
    return buf
}
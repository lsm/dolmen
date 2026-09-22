package store

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
)

const incarnationTokenVersion = 1

func EncodeIncarnation(inc Incarnation) string {
	raw := make([]byte, 0, 1+16+8+8+2+len(inc.Table))
	raw = append(raw, incarnationTokenVersion)
	raw = append(raw, inc.NsGen[:]...)
	raw = binary.BigEndian.AppendUint64(raw, uint64(inc.Version))
	raw = binary.BigEndian.AppendUint64(raw, uint64(inc.DropGen))
	raw = binary.BigEndian.AppendUint16(raw, uint16(len(inc.Table)))
	raw = append(raw, inc.Table...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func DecodeIncarnation(token string) (Incarnation, error) {
	bad := fmt.Errorf("%w: expected_incarnation is not a token this server issued; take it from the dry run's response verbatim", ErrInvalid)
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < 1+16+8+8+2 || raw[0] != incarnationTokenVersion {
		return Incarnation{}, bad
	}
	var inc Incarnation
	copy(inc.NsGen[:], raw[1:17])
	inc.Version = int64(binary.BigEndian.Uint64(raw[17:25]))
	inc.DropGen = int64(binary.BigEndian.Uint64(raw[25:33]))
	nameLen := int(binary.BigEndian.Uint16(raw[33:35]))
	if len(raw) != 35+nameLen {
		return Incarnation{}, bad
	}
	inc.Table = string(raw[35:])
	return inc, nil
}

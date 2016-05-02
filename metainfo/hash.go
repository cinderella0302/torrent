package metainfo

import (
	"encoding/hex"
	"fmt"
)

// 20-byte SHA1 hash used for info and pieces.
type Hash [20]byte

func (h Hash) Bytes() []byte {
	return h[:]
}

func (h *Hash) AsString() string {
	return string(h[:])
}

func (h Hash) HexString() string {
	return fmt.Sprintf("%x", h[:])
}

func (h *Hash) FromHexString(s string) (err error) {
	if len(s) != 40 {
		err = fmt.Errorf("hash hex string has bad length: %d", len(s))
		return
	}
	n, err := hex.Decode(h[:], []byte(s))
	if err != nil {
		return
	}
	if n != 20 {
		panic(n)
	}
	return
}

// Package oauth2cli provides ...
package oauth2cli

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

func newOAuth2State() (string, error) {
	var n uint64
	if err := binary.Read(rand.Reader, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", n), nil
}

package main

import (
	"crypto/des"
	"encoding/hex"
	"errors"
	"strings"
)

const serverListKey = "dw!@#$%^"

func decryptServerList(data []byte) ([]byte, error) {
	s := strings.TrimSpace(string(data))
	ct, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(ct) == 0 || len(ct)%des.BlockSize != 0 {
		return nil, errors.New("invalid DES ciphertext length")
	}
	block, err := des.NewCipher([]byte(serverListKey))
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(ct))
	for i := 0; i < len(ct); i += des.BlockSize {
		block.Decrypt(out[i:i+des.BlockSize], ct[i:i+des.BlockSize])
	}
	pad := int(out[len(out)-1])
	if pad > 0 && pad <= des.BlockSize && pad <= len(out) {
		out = out[:len(out)-pad]
	}
	return out, nil
}

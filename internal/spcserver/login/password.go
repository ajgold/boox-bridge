// Package login holds the SPC terminal-login verification core: the one-time
// randomCode store, the password-hash recipe, and userId resolution. It is
// device-facing crypto, kept independent of the HTTP handlers.
package login

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Md5Hex returns the lowercase zero-padded hex MD5 of s over UTF-8 bytes,
// matching SPC's MD5StrUtil.
func Md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Sha256Hex returns the lowercase zero-padded hex SHA-256 of s over UTF-8
// bytes, matching SPC's SHA256Util.byte2Hex.
func Sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ServicePassword is the stored form of the account password: md5Hex(raw).
// UB stores the raw password and derives this at validation time.
func ServicePassword(raw string) string { return Md5Hex(raw) }

// CheckWebPassword reports whether the device-sent webPassword matches the SPC
// recipe for the given raw account password and one-time code:
//
//	webPassword == sha256Hex( md5Hex(rawPassword) + code )
//
// The comparison trims surrounding whitespace from webPassword (the device may
// pad it). See docs/spc-protocol.md §2.1.
func CheckWebPassword(rawPassword, code, webPassword string) bool {
	want := Sha256Hex(ServicePassword(rawPassword) + code)
	return strings.TrimSpace(webPassword) == want
}

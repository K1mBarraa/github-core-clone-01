package main

// Small shared helpers: hashing, base64, deterministic time, JSON.
// Mirrors util.ts behaviour exactly (status codes aside, these are pure).

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Sha1Hex returns the SHA-1 of data as a lowercase 40-hex string.
func Sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// ZeroOID is the all-zero object id.
const ZeroOID = "0000000000000000000000000000000000000000"

// IsOID reports whether s is a 40-hex object id.
func IsOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		hexDigit := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !hexDigit {
			return false
		}
	}
	return true
}

// B64Encode returns standard base64 of UTF-8 text.
func B64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// B64EncodeBytes returns standard base64 of bytes.
func B64EncodeBytes(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// B64Decode decodes base64, ignoring whitespace first (GitHub wraps
// content lines). It returns an error on malformed input — but still
// returns the bytes decoded before the bad input, exactly as
// encoding/base64.DecodeString does: callers that ignore the error (as
// the TS reference, where Node never throws on bad base64 chars, would)
// still get the usable prefix.
func B64Decode(s string) ([]byte, error) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\r', '\n', '\t':
			return -1
		default:
			return r
		}
	}, s)
	return base64.StdEncoding.DecodeString(cleaned)
}

// B64URL returns base64url (no padding) of UTF-8 text.
func B64URL(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// The trial's clock, read once at startup: MAKEFUN_TRIAL_NOW is the
// trial's "now" in milliseconds when the harness sets it; otherwise the
// scenario's epoch, so a run never reads the wall clock for data.
var trialNowSecValue = readTrialNowSec()

func readTrialNowSec() int64 {
	raw := strings.TrimSpace(os.Getenv("MAKEFUN_TRIAL_NOW"))
	if raw == "" {
		return 1735689600
	}
	// strconv.ParseFloat rejects surrounding whitespace, while JS
	// Number() trims it, so trim first and then parse strictly: a
	// value with trailing junk (e.g. "123abc") must fall back, exactly
	// as Number() returning NaN would.
	ms, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(ms) || math.IsInf(ms, 0) || ms <= 0 {
		return 1735689600
	}
	return int64(ms / 1000)
}

// TrialNowSec returns the trial clock in seconds.
func TrialNowSec() int64 {
	return trialNowSecValue
}

// IsoOf returns the ISO-8601 UTC form of epochSec, identical to JS
// Date.toISOString: milliseconds are always present, e.g.
// 2025-01-01T00:00:00.000Z.
func IsoOf(sec int64) string {
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// JSONText serializes v to JSON and never throws: an unserializable value
// falls back to a quoted placeholder, and a nil value becomes "null".
// HTML characters are left unescaped, matching JSON.stringify.
func JSONText(v any) string {
	if v == nil {
		return "null"
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return `"<unserializable>"`
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

var requestCounter int64

// NextRequestID returns a short, stable request id: `req-<counter>`.
func NextRequestID() string {
	n := atomic.AddInt64(&requestCounter, 1)
	return "req-" + strconv.FormatInt(n, 10)
}

// CmpStr compares two strings byte-wise: -1, 0 or 1.
func CmpStr(a, b string) int {
	return strings.Compare(a, b)
}

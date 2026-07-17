package genai

import (
	"strconv"
	"strings"
)

// toString renders a JSON scalar as a string ("" for nil / non-scalar).
func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// Integers arrive as float64 via encoding/json; avoid a trailing ".0".
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

// toInt extracts an integer from a JSON number (float64) or numeric string.
func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case string:
		if n, err := strconv.Atoi(t); err == nil {
			return n, true
		}
	}
	return 0, false
}

// toFloat extracts a float from a JSON number or numeric string.
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// clamp truncates s to at most n bytes on a UTF-8 boundary.
func clamp(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back up to a rune boundary.
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// containsFold reports whether list already holds s (case-insensitive).
func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

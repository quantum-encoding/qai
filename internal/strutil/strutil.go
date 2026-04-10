// Package strutil provides shared string utilities.
package strutil

import "strings"

// TruncateStr truncates a string to max characters.
func TruncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// EscapeSurQL escapes a string for SurrealQL.
func EscapeSurQL(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	return s
}

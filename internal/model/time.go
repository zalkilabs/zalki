package model

import "time"

// Timestamp returns one current clock sample in UTC with exactly nine
// fractional digits, preserving lexicographic journal ordering. It formats a
// value; it does not change any session, message or receipt state.
func Timestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z") }

package model

import (
	"strings"
	"unicode/utf8"
)

// ValidateMessageBody checks the wire body limit and prevents capability disclosure.
func ValidateMessageBody(body, key string) error {
	if !utf8.ValidString(body) {
		return NewError(CodeInvalidInput, "body is not valid UTF-8")
	}
	if len(body) == 0 || len(body) > 16*1024 || strings.ContainsRune(body, 0) {
		return NewError(CodeInvalidInput, "body must be 1..16384 UTF-8 bytes without NUL")
	}
	if key != "" && strings.Contains(body, key) {
		return NewError(CodeInvalidInput, "refusing to include your session capability in an agent message")
	}
	return nil
}

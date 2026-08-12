package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var ErrInvalidWorkspaceKey = errors.New("invalid_workspace_key")

func Key(identifier string) (string, error) {
	if identifier == "" || identifier == "." || identifier == ".." {
		return "", fmt.Errorf("%w: identifier cannot name the workspace root", ErrInvalidWorkspaceKey)
	}

	var key strings.Builder
	changed := false
	for _, character := range identifier {
		if workspaceKeyCharacter(character) {
			key.WriteRune(character)
			continue
		}
		key.WriteByte('_')
		changed = true
	}
	if key.Len() == 0 || key.String() == "." || key.String() == ".." {
		return "", fmt.Errorf("%w: sanitized identifier cannot name the workspace root", ErrInvalidWorkspaceKey)
	}
	if !changed {
		return key.String(), nil
	}
	digest := sha256.Sum256([]byte(identifier))
	return key.String() + "-" + hex.EncodeToString(digest[:8]), nil
}

func workspaceKeyCharacter(character rune) bool {
	return character >= 'A' && character <= 'Z' ||
		character >= 'a' && character <= 'z' ||
		character >= '0' && character <= '9' ||
		character == '.' || character == '_' || character == '-'
}

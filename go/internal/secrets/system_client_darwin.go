//go:build darwin

package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os/exec"
	"strings"
)

const (
	securityPath             = "/usr/bin/security"
	legacyEncodingPrefix     = "go-keyring-encoded:"
	compatibleEncodingPrefix = "go-keyring-base64:"
	maxSecurityCommandBytes  = 4096
)

var (
	errCredentialTooLarge       = errors.New("native keyring credential exceeds command limit")
	errInvalidKeyringIdentifier = errors.New("native keyring identifier contains unsupported control character")
)

type securityRunner interface {
	Run(context.Context, []byte, string, ...string) ([]byte, error)
}

type execSecurityRunner struct{}

func (execSecurityRunner) Run(ctx context.Context, stdin []byte, path string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	output, err := cmd.CombinedOutput()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return output, ctxErr
	}
	return output, err
}

type macOSKeyringClient struct {
	runner securityRunner
}

func newSystemKeyringClient() keyringClient {
	return newMacOSKeyringClient(execSecurityRunner{})
}

func newMacOSKeyringClient(runner securityRunner) keyringClient {
	return &macOSKeyringClient{runner: runner}
}

func (c *macOSKeyringClient) Set(ctx context.Context, service, account, password string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateKeyringIdentifiers(service, account); err != nil {
		return err
	}

	passwordBytes := []byte(password)
	encodedPassword := make([]byte, len(compatibleEncodingPrefix)+base64.StdEncoding.EncodedLen(len(passwordBytes)))
	copy(encodedPassword, compatibleEncodingPrefix)
	base64.StdEncoding.Encode(encodedPassword[len(compatibleEncodingPrefix):], passwordBytes)
	clear(passwordBytes)
	defer clear(encodedPassword)

	command := make([]byte, 0, len(service)+len(account)+len(encodedPassword)+48)
	command = append(command, "add-generic-password -U -s "...)
	command = append(command, quoteSecurityToken(service)...)
	command = append(command, " -a "...)
	command = append(command, quoteSecurityToken(account)...)
	command = append(command, " -w "...)
	command = append(command, encodedPassword...)
	command = append(command, '\n')
	defer clear(command)
	if len(command) > maxSecurityCommandBytes {
		return errCredentialTooLarge
	}

	output, err := c.runner.Run(ctx, command, securityPath, "-i")
	clear(output)
	return err
}

func (c *macOSKeyringClient) Get(ctx context.Context, service, account string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateKeyringIdentifiers(service, account); err != nil {
		return "", err
	}

	output, err := c.runner.Run(ctx, nil, securityPath,
		"find-generic-password", "-s", service, "-wa", account,
	)
	defer clear(output)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if keychainItemNotFound(output) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}

	value := bytes.TrimSpace(output)
	if bytes.HasPrefix(value, []byte(compatibleEncodingPrefix)) {
		encoded := value[len(compatibleEncodingPrefix):]
		decoded := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
		decodedBytes, decodeErr := base64.StdEncoding.Decode(decoded, encoded)
		if decodeErr != nil {
			clear(decoded)
			return "", decodeErr
		}
		result := string(decoded[:decodedBytes])
		clear(decoded)
		return result, nil
	}
	if bytes.HasPrefix(value, []byte(legacyEncodingPrefix)) {
		encoded := value[len(legacyEncodingPrefix):]
		decoded := make([]byte, hex.DecodedLen(len(encoded)))
		decodedBytes, decodeErr := hex.Decode(decoded, encoded)
		if decodeErr != nil {
			clear(decoded)
			return "", decodeErr
		}
		result := string(decoded[:decodedBytes])
		clear(decoded)
		return result, nil
	}
	return string(value), nil
}

func (c *macOSKeyringClient) Delete(ctx context.Context, service, account string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateKeyringIdentifiers(service, account); err != nil {
		return err
	}

	output, err := c.runner.Run(ctx, nil, securityPath,
		"delete-generic-password", "-s", service, "-a", account,
	)
	defer clear(output)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if keychainItemNotFound(output) {
		return ErrNotFound
	}
	return err
}

func keychainItemNotFound(output []byte) bool {
	return bytes.Contains(output, []byte("could not be found"))
}

func validateKeyringIdentifiers(values ...string) error {
	for _, value := range values {
		for i := 0; i < len(value); i++ {
			if value[i] < 0x20 || value[i] == 0x7f {
				return errInvalidKeyringIdentifier
			}
		}
	}
	return nil
}

func quoteSecurityToken(value string) string {
	if value == "" {
		return "''"
	}
	if strings.IndexFunc(value, func(r rune) bool {
		return !isSecurityTokenCharacter(r)
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func isSecurityTokenCharacter(r rune) bool {
	return r >= 'a' && r <= 'z' ||
		r >= 'A' && r <= 'Z' ||
		r >= '0' && r <= '9' ||
		r == '_' || strings.ContainsRune("@%+=:,./-", r)
}

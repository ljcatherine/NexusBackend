package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	secretMaxFileBytes = 4096
	secretMinLength    = 32
)

// secretLoadCredential reads a systemd LoadCredential entry from $CREDENTIALS_DIRECTORY.
func secretLoadCredential(name string) (string, error) {
	if !secretSafeName(name) {
		return "", errors.New("credential name must be a single safe path segment")
	}
	directory := os.Getenv("CREDENTIALS_DIRECTORY")
	if directory == "" || !filepath.IsAbs(directory) {
		return "", errors.New("CREDENTIALS_DIRECTORY is not set to an absolute path")
	}
	return secretLoadFile(filepath.Join(directory, name))
}

// secretLoadFile reads a bearer secret from an explicit absolute path.
// The value is never included in returned errors.
func secretLoadFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("secret file path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("secret file could not be read")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("secret file must be a regular file without symlinks")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("secret file could not be read")
	}
	defer file.Close()
	// Check the opened handle as well, so the file cannot be swapped after Lstat.
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		return "", errors.New("secret file must be a regular file without symlinks")
	}
	if runtime.GOOS != "windows" && opened.Mode().Perm()&0o077 != 0 {
		return "", errors.New("secret file must not be readable by group or others")
	}
	data, err := io.ReadAll(io.LimitReader(file, secretMaxFileBytes+1))
	if err != nil {
		return "", errors.New("secret file could not be read")
	}
	if len(data) > secretMaxFileBytes {
		return "", errors.New("secret file exceeds size limit")
	}
	return secretValidate(strings.TrimSuffix(string(data), "\n"))
}

// secretValidate accepts printable ASCII only, matching validOrchestratorToken.
func secretValidate(token string) (string, error) {
	if token == "" {
		return "", errors.New("secret is empty")
	}
	if len(token) < secretMinLength {
		return "", errors.New("secret must be at least 32 characters")
	}
	for i := 0; i < len(token); i++ {
		if token[i] <= ' ' || token[i] > '~' {
			return "", errors.New("secret contains whitespace or control characters")
		}
	}
	return token, nil
}

func secretSafeName(name string) bool {
	if name == "" || len(name) > 255 || name == "." || name == ".." {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
			return false
		}
	}
	return true
}

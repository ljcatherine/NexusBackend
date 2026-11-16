package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const secretTestToken = "test-token-0123456789abcdef0123456789abcdef"

func secretWriteFile(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ps5-token")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("write secret fixture: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod secret fixture: %v", err)
	}
	return path
}

func TestSecretsLoadsCredentialFromCredentialsDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "nexus-ps5-token"), []byte(secretTestToken+"\n"), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", directory)

	token, err := secretLoadCredential("nexus-ps5-token")
	if err != nil || token != secretTestToken {
		t.Fatalf("credential load = %q, %v; want the fixture token", token, err)
	}
}

func TestSecretsCredentialNameMustBeSafePathSegment(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
	for _, name := range []string{"", ".", "..", "../ps5-token", "nested/ps5-token", `nested\ps5-token`, "C:token", "bad name"} {
		if _, err := secretLoadCredential(name); err == nil {
			t.Fatalf("credential name %q was accepted", name)
		}
	}
}

func TestSecretsCredentialRequiresAbsoluteCredentialsDirectory(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if _, err := secretLoadCredential("nexus-ps5-token"); err == nil {
		t.Fatal("empty CREDENTIALS_DIRECTORY was accepted")
	}
	t.Setenv("CREDENTIALS_DIRECTORY", "relative/dir")
	if _, err := secretLoadCredential("nexus-ps5-token"); err == nil {
		t.Fatal("relative CREDENTIALS_DIRECTORY was accepted")
	}
}

func TestSecretsLoadsExplicitAbsoluteFile(t *testing.T) {
	path := secretWriteFile(t, secretTestToken+"\n", 0o600)
	token, err := secretLoadFile(path)
	if err != nil || token != secretTestToken {
		t.Fatalf("file load = %q, %v; want the fixture token", token, err)
	}
	if _, err := secretLoadFile("relative/ps5-token"); err == nil {
		t.Fatal("relative file path was accepted")
	}
}

func TestSecretsTrimsExactlyOneTrailingNewline(t *testing.T) {
	path := secretWriteFile(t, secretTestToken+"\n\n", 0o600)
	if _, err := secretLoadFile(path); err == nil {
		t.Fatal("token followed by two newlines was accepted")
	}
	path = secretWriteFile(t, secretTestToken, 0o600)
	if token, err := secretLoadFile(path); err != nil || token != secretTestToken {
		t.Fatalf("token without newline = %q, %v", token, err)
	}
}

func TestSecretsRejectsShortToken(t *testing.T) {
	short := "test-token-0123456789abcdef"
	path := secretWriteFile(t, short+"\n", 0o600)
	_, err := secretLoadFile(path)
	if err == nil {
		t.Fatal("short token was accepted")
	}
	if strings.Contains(err.Error(), short) {
		t.Fatalf("error leaked token value: %v", err)
	}
}

func TestSecretsRejectsEmptyFile(t *testing.T) {
	if _, err := secretLoadFile(secretWriteFile(t, "\n", 0o600)); err == nil {
		t.Fatal("empty file was accepted")
	}
	if _, err := secretLoadFile(secretWriteFile(t, "", 0o600)); err == nil {
		t.Fatal("zero-byte file was accepted")
	}
}

func TestSecretsRejectsWhitespaceAndControlCharacters(t *testing.T) {
	cases := map[string]string{
		"space":        secretTestToken[:20] + " " + secretTestToken[20:],
		"tab":          secretTestToken[:20] + "\t" + secretTestToken[20:],
		"carriage":     secretTestToken + "\r",
		"embedded_nul": secretTestToken[:20] + "\x00" + secretTestToken[20:],
		"leading_nl":   "\n" + secretTestToken,
		"non_ascii":    secretTestToken[:20] + "é" + secretTestToken[20:],
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := secretLoadFile(secretWriteFile(t, contents, 0o600))
			if err == nil {
				t.Fatal("token with whitespace or control characters was accepted")
			}
			if strings.Contains(err.Error(), secretTestToken[:20]) {
				t.Fatalf("error leaked token value: %v", err)
			}
		})
	}
}

func TestSecretsRejectsOversizedFile(t *testing.T) {
	oversized := secretTestToken + strings.Repeat("a", secretMaxFileBytes)
	_, err := secretLoadFile(secretWriteFile(t, oversized, 0o600))
	if err == nil {
		t.Fatal("oversized file was accepted")
	}
	if strings.Contains(err.Error(), secretTestToken) {
		t.Fatalf("error leaked token value: %v", err)
	}
}

func TestSecretsRejectsGroupOrOtherReadableFileOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		path := secretWriteFile(t, secretTestToken+"\n", mode)
		if _, err := secretLoadFile(path); err == nil {
			t.Fatalf("file with mode %o was accepted", mode)
		}
	}
}

func TestSecretsRejectsSymlink(t *testing.T) {
	target := secretWriteFile(t, secretTestToken+"\n", 0o600)
	link := filepath.Join(t.TempDir(), "ps5-token")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := secretLoadFile(link); err == nil {
		t.Fatal("symlink was accepted")
	}
}

func TestSecretsErrorsNeverContainContent(t *testing.T) {
	bad := []string{
		secretTestToken[:10] + " " + secretTestToken[10:],
		secretTestToken[:10],
		strings.Repeat(secretTestToken, 200),
	}
	for _, contents := range bad {
		_, err := secretLoadFile(secretWriteFile(t, contents, 0o600))
		if err == nil {
			t.Fatalf("invalid content was accepted: %d bytes", len(contents))
		}
		if strings.Contains(err.Error(), secretTestToken[:10]) {
			t.Fatalf("error leaked content: %v", err)
		}
	}
	if _, err := secretLoadFile(filepath.Join(t.TempDir(), "missing")); err == nil || strings.Contains(err.Error(), secretTestToken) {
		t.Fatalf("missing file error = %v", err)
	}
}

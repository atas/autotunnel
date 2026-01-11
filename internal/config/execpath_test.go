package config

import (
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestDefaultExecPaths(t *testing.T) {
	paths := DefaultExecPaths()

	if len(paths) == 0 {
		t.Error("expected at least one default path")
	}

	// Check that /usr/local/bin is included on all platforms
	if !slices.Contains(paths, "/usr/local/bin") {
		t.Error("expected /usr/local/bin in default paths")
	}

	// Check platform-specific paths
	if runtime.GOOS == "darwin" {
		if !slices.Contains(paths, "/opt/homebrew/bin") {
			t.Error("expected /opt/homebrew/bin on darwin")
		}
	}

	if runtime.GOOS == "linux" {
		if !slices.Contains(paths, "/snap/bin") {
			t.Error("expected /snap/bin on linux")
		}
	}
}

func TestExpandExecPath(t *testing.T) {
	// Save and restore PATH
	originalPath := os.Getenv("PATH")
	defer os.Setenv("PATH", originalPath)

	// Set a minimal PATH
	os.Setenv("PATH", "/usr/bin:/bin")

	// Create a temp directory to use as a "valid" path
	tmpDir := t.TempDir()

	ExpandExecPath([]string{tmpDir})

	newPath := os.Getenv("PATH")
	if !strings.HasPrefix(newPath, tmpDir) {
		t.Errorf("expected PATH to start with %s, got %s", tmpDir, newPath)
	}

	// Verify original path is preserved at the end
	if !strings.HasSuffix(newPath, "/usr/bin:/bin") {
		t.Errorf("expected PATH to end with /usr/bin:/bin, got %s", newPath)
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home directory")
	}

	tests := []struct {
		input    string
		expected string
	}{
		{"~/bin", home + "/bin"},
		{"~/.local/bin", home + "/.local/bin"},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
	}

	for _, tt := range tests {
		result := expandTilde(tt.input)
		if result != tt.expected {
			t.Errorf("expandTilde(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestExpandExecPath_NoDuplicates(t *testing.T) {
	originalPath := os.Getenv("PATH")
	defer os.Setenv("PATH", originalPath)

	// Create a temp directory
	tmpDir := t.TempDir()

	// Set PATH with tmpDir already present
	os.Setenv("PATH", tmpDir+":/usr/bin")

	ExpandExecPath([]string{tmpDir})

	newPath := os.Getenv("PATH")
	count := strings.Count(newPath, tmpDir)
	if count > 1 {
		t.Errorf("expected %s to appear once, appeared %d times in %s", tmpDir, count, newPath)
	}
}

func TestExpandExecPath_SkipsNonExistent(t *testing.T) {
	originalPath := os.Getenv("PATH")
	defer os.Setenv("PATH", originalPath)

	os.Setenv("PATH", "/usr/bin")

	// Try to add a path that doesn't exist
	ExpandExecPath([]string{"/this/path/does/not/exist/xyz123"})

	newPath := os.Getenv("PATH")
	if strings.Contains(newPath, "/this/path/does/not/exist/xyz123") {
		t.Error("non-existent path should not be added to PATH")
	}
}

func TestExpandExecPath_EmptyInput(t *testing.T) {
	originalPath := os.Getenv("PATH")
	defer os.Setenv("PATH", originalPath)

	os.Setenv("PATH", "/usr/bin:/bin")

	// Call with empty additional paths - should still add default paths if they exist
	ExpandExecPath(nil)

	// Just verify no panic and PATH is still valid
	newPath := os.Getenv("PATH")
	if newPath == "" {
		t.Error("PATH should not be empty after ExpandExecPath")
	}
}

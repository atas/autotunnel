package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultExecPaths returns the default paths to add for exec credential plugins
// based on the current operating system. These paths are commonly used for
// installing tools like kubectl, aws-iam-authenticator, gcloud, etc.
func DefaultExecPaths() []string {
	home, _ := os.UserHomeDir()

	var paths []string

	switch runtime.GOOS {
	case "darwin":
		paths = []string{
			"/usr/local/bin",
			"/opt/homebrew/bin",
			"/opt/homebrew/sbin",
		}
	case "linux":
		paths = []string{
			"/usr/local/bin",
			"/usr/local/sbin",
			"/snap/bin",
		}
	default:
		paths = []string{
			"/usr/local/bin",
		}
	}

	// Add home-relative paths (common across platforms)
	if home != "" {
		paths = append(paths,
			filepath.Join(home, "bin"),
			filepath.Join(home, ".local", "bin"),
		)
	}

	return paths
}

// ExpandExecPath prepends the given paths and default paths to the current PATH.
// It filters out paths that don't exist and avoids duplicates.
// This is useful when running as a service where PATH is minimal.
func ExpandExecPath(additionalPaths []string) {
	currentPath := os.Getenv("PATH")
	existingPaths := make(map[string]bool)

	// Track existing paths to avoid duplicates
	for _, p := range strings.Split(currentPath, string(os.PathListSeparator)) {
		existingPaths[p] = true
	}

	var newPaths []string

	// Process additional paths from config (user-specified first)
	for _, p := range additionalPaths {
		p = expandTilde(p)
		if _, err := os.Stat(p); err == nil {
			if !existingPaths[p] {
				newPaths = append(newPaths, p)
				existingPaths[p] = true
			}
		}
	}

	// Process default paths
	for _, p := range DefaultExecPaths() {
		if _, err := os.Stat(p); err == nil {
			if !existingPaths[p] {
				newPaths = append(newPaths, p)
				existingPaths[p] = true
			}
		}
	}

	// Prepend new paths to existing PATH
	if len(newPaths) > 0 {
		expandedPath := strings.Join(newPaths, string(os.PathListSeparator))
		if currentPath != "" {
			expandedPath = expandedPath + string(os.PathListSeparator) + currentPath
		}
		os.Setenv("PATH", expandedPath)
	}
}

// expandTilde expands ~ to the user's home directory
func expandTilde(path string) string {
	if len(path) > 0 && path[0] == '~' {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

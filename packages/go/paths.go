package easysql

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const LibraryPathEnv = "EASYSQL_LIBRARY_PATH"

func libraryFileName() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "libeasysql.dylib", nil
	case "linux/amd64":
		return "libeasysql.so", nil
	case "windows/amd64":
		return "easysql.dll", nil
	default:
		return "", fmt.Errorf("easysql: no native SDK for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

func defaultLibraryCandidates() ([]string, error) {
	name, err := libraryFileName()
	if err != nil {
		return nil, err
	}
	var candidates []string
	if configured := os.Getenv(LibraryPathEnv); configured != "" {
		candidates = append(candidates, configured)
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, name),
			filepath.Join(cwd, "lib", name),
		)
	}
	if executable, err := os.Executable(); err == nil {
		directory := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(directory, name),
			filepath.Join(directory, "lib", name),
		)
	}
	candidates = append(candidates, name)
	return deduplicatePaths(candidates), nil
}

func deduplicatePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		key := path
		if filepath.Base(path) == path {
			key = "loader:" + path
		} else if absolute, err := filepath.Abs(path); err == nil {
			key = absolute
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, path)
	}
	return result
}

package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxPathCompletions = 100

// projectPaths returns directory candidates for the add-project input. An
// empty input starts in the server user's home directory, which is also the
// natural place to begin when using the ~ shorthand.
func (s *Server) projectPaths(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		http.Error(w, "could not find home directory", http.StatusInternalServerError)
		return
	}
	paths := completeProjectPaths(r.URL.Query().Get("path"), home)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(paths)
}

func completeProjectPaths(input, home string) []string {
	input = strings.TrimSpace(input)
	dir, prefix, displayDir, ok := completionParts(input, home)
	if !ok {
		return []string{}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{}
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) || !isDir(entry) {
			continue
		}
		paths = append(paths, filepath.Join(displayDir, entry.Name())+string(filepath.Separator))
	}
	sort.Slice(paths, func(i, j int) bool {
		// Keep ordinary project directories ahead of hidden configuration
		// directories while retaining a stable alphabetical order.
		iHidden, jHidden := strings.HasPrefix(filepath.Base(strings.TrimSuffix(paths[i], string(filepath.Separator))), "."), strings.HasPrefix(filepath.Base(strings.TrimSuffix(paths[j], string(filepath.Separator))), ".")
		if iHidden != jHidden {
			return !iHidden
		}
		return paths[i] < paths[j]
	})
	if len(paths) > maxPathCompletions {
		paths = paths[:maxPathCompletions]
	}
	return paths
}

func completionParts(input, home string) (dir, prefix, displayDir string, ok bool) {
	if input == "" || input == "~" || input == "~/" {
		return home, "", "~", true
	}

	if strings.HasPrefix(input, "~/") {
		expanded := filepath.Join(home, input[2:])
		if strings.HasSuffix(input, string(filepath.Separator)) {
			dir, prefix = filepath.Clean(expanded), ""
		} else {
			dir, prefix = splitCompletion(expanded)
		}
		rel, err := filepath.Rel(home, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return dir, prefix, dir, true
		}
		if rel == "." {
			return dir, prefix, "~", true
		}
		return dir, prefix, filepath.Join("~", rel), true
	}
	if filepath.IsAbs(input) {
		dir, prefix = splitCompletion(input)
		return dir, prefix, dir, true
	}
	return "", "", "", false
}

func splitCompletion(input string) (dir, prefix string) {
	if strings.HasSuffix(input, string(filepath.Separator)) {
		return filepath.Clean(input), ""
	}
	return filepath.Dir(input), filepath.Base(input)
}

func isDir(entry os.DirEntry) bool {
	if entry.IsDir() {
		return true
	}
	if entry.Type()&os.ModeSymlink == 0 {
		return false
	}
	info, err := entry.Info()
	return err == nil && info.IsDir()
}

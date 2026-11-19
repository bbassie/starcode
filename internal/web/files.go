package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxUploadSize caps the decoded bytes of one prompt's attachments.
const maxUploadSize = 25 << 20

// UploadFile is what Datastar puts in the signal for a bound file input.
type UploadFile struct {
	Name     string `json:"name"`
	Contents string `json:"contents"` // base64
	Mime     string `json:"mime"`
}

// saveAttachments stores prompt attachments under dir (created on demand)
// and returns their absolute paths for the agent to read.
func saveAttachments(dir string, files []UploadFile) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return writeUploads(dir, files)
}

func writeUploads(dir string, files []UploadFile) ([]string, error) {
	total := 0
	paths := make([]string, 0, len(files))
	for _, f := range files {
		data, err := base64.StdEncoding.DecodeString(f.Contents)
		if err != nil {
			return nil, fmt.Errorf("%s: bad file data", f.Name)
		}
		total += len(data)
		if total > maxUploadSize {
			return nil, fmt.Errorf("upload larger than %d MiB", maxUploadSize>>20)
		}
		name := filepath.Base(strings.ReplaceAll(f.Name, "\\", "/"))
		if name == "" || name == "." || name == ".." || name == ".git" || name == string(filepath.Separator) {
			return nil, fmt.Errorf("bad file name %q", f.Name)
		}
		path := uniquePath(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// promptWithAttachments appends one self-contained path line per saved file
// (same wording t3 code uses) so the agent knows to read them with its own
// tools; Claude Code renders images it reads from disk. files and paths run
// parallel: files carries the mime type, paths where each one landed.
func promptWithAttachments(text string, files []UploadFile, paths []string) string {
	lines := make([]string, len(paths))
	for i, p := range paths {
		kind := "file"
		if strings.HasPrefix(files[i].Mime, "image/") {
			kind = "image"
		}
		lines[i] = fmt.Sprintf("[Attached %s %q is saved at: %s]", kind, filepath.Base(p), p)
	}
	if text == "" {
		return strings.Join(lines, "\n")
	}
	return text + "\n\n" + strings.Join(lines, "\n")
}

func uniquePath(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	path := filepath.Join(dir, name)
	for i := 1; ; i++ {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return path
		}
		path = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
	}
}

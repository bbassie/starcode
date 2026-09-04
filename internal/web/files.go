package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxEditableFileSize = 1 << 20  // 1 MiB keeps the editor responsive.
	maxUploadSize       = 25 << 20 // decoded bytes per upload request
)

// UploadFile is what Datastar puts in the signal for a bound file input.
type UploadFile struct {
	Name     string `json:"name"`
	Contents string `json:"contents"` // base64
	Mime     string `json:"mime"`
}

// saveUploads writes browser-picked files into dir (relative to the project
// root). Names are flattened to their base name and never overwrite: an
// existing shot.png makes the next one shot-1.png.
func saveUploads(root, dir string, files []UploadFile) error {
	target, info, _, err := resolveProjectPath(root, dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("upload target is not a directory")
	}
	_, err = writeUploads(target, files)
	return err
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

type projectDirEntry struct {
	Name  string
	Path  string
	IsDir bool
}

func listProjectDir(root, path string) (string, []projectDirEntry, error) {
	dir, info, clean, err := resolveProjectPath(root, path)
	if err != nil {
		return "", nil, err
	}
	if !info.IsDir() {
		return "", nil, errors.New("path is not a directory")
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, err
	}
	entries := make([]projectDirEntry, 0, len(items))
	for _, item := range items {
		child := filepath.Join(clean, item.Name())
		_, childInfo, childClean, err := resolveProjectPath(root, child)
		if err != nil {
			// Hide broken links and links that leave the project.
			continue
		}
		entries = append(entries, projectDirEntry{
			Name:  item.Name(),
			Path:  filepath.ToSlash(childClean),
			IsDir: childInfo.IsDir(),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return filepath.ToSlash(clean), entries, nil
}

func readProjectFile(root, path string) (string, error) {
	file, info, err := projectFile(root, path)
	if err != nil {
		return "", err
	}
	if info.Size() > maxEditableFileSize {
		return "", fmt.Errorf("file is too large to edit here (maximum %d MiB)", maxEditableFileSize>>20)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if strings.IndexByte(string(data), 0) >= 0 {
		return "", errors.New("binary files cannot be edited here")
	}
	return string(data), nil
}

func writeProjectFile(root, path, content string) error {
	if len(content) > maxEditableFileSize {
		return fmt.Errorf("file is too large to edit here (maximum %d MiB)", maxEditableFileSize>>20)
	}
	file, info, err := projectFile(root, path)
	if err != nil {
		return err
	}
	if strings.IndexByte(content, 0) >= 0 {
		return errors.New("binary file content is not supported")
	}
	return os.WriteFile(file, []byte(content), info.Mode().Perm())
}

// projectFile confines file reads and writes to the real project directory,
// including when a path points through a symlink.
func projectFile(root, path string) (string, os.FileInfo, error) {
	file, info, _, err := resolveProjectPath(root, path)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, errors.New("only regular files can be edited")
	}
	return file, info, nil
}

func resolveProjectPath(root, path string) (string, os.FileInfo, string, error) {
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." {
		clean = ""
	}
	if clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", nil, "", errors.New("file path must stay inside the project")
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".git" {
			return "", nil, "", errors.New("the .git directory cannot be opened here")
		}
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, "", err
	}
	file, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", nil, "", err
	}
	rel, err := filepath.Rel(root, file)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", nil, "", errors.New("file path must stay inside the project")
	}
	info, err := os.Stat(file)
	if err != nil {
		return "", nil, "", err
	}
	return file, info, clean, nil
}

package web

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxEditableFileSize = 1 << 20 // 1 MiB keeps the editor responsive.

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
	path = filepath.Clean(path)
	if path == "." || path == ".." || filepath.IsAbs(path) || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return "", nil, errors.New("file path must stay inside the project")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, err
	}
	file, err := filepath.EvalSymlinks(filepath.Join(root, path))
	if err != nil {
		return "", nil, err
	}
	rel, err := filepath.Rel(root, file)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", nil, errors.New("file path must stay inside the project")
	}
	info, err := os.Stat(file)
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, errors.New("only regular files can be edited")
	}
	return file, info, nil
}

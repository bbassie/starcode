package web

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxEditableFileSize = 1 << 20 // 1 MiB keeps the editor responsive.

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

// Package gitx reads git state by shelling out to git. It is read-only.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && out.Len() > 0 {
		// git diff exits 1 when there are differences (with --no-index,
		// and with --exit-code). That is output, not failure.
		return out.String(), nil
	}
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

// Branch is the branch checked out in dir, "HEAD" when it is detached,
// or "" when dir is not in a repository.
func Branch(ctx context.Context, dir string) string {
	b, err := run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(b)
}

// maxListed caps the fallback walk in ListFiles for a directory that is
// not a repository.
const maxListed = 5000

// ListFiles names every file under dir relative to it: tracked and
// untracked-but-not-ignored ones in a repository, a bounded walk of the
// tree otherwise. Paths use forward slashes.
func ListFiles(ctx context.Context, dir string) []string {
	if out, err := run(ctx, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard"); err == nil {
		var files []string
		for _, p := range strings.Split(out, "\x00") {
			if p != "" {
				files = append(files, p)
			}
		}
		return files
	}
	var files []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || (path != dir && strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= maxListed {
			return filepath.SkipAll
		}
		if rel, err := filepath.Rel(dir, path); err == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files
}

// MatchFiles picks the paths that contain every space-separated word of
// q, base name matches first and shorter paths before longer ones.
func MatchFiles(files []string, q string, limit int) []string {
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 {
		return nil
	}
	type hit struct {
		path string
		rank int
	}
	var hits []hit
	for _, p := range files {
		lp := strings.ToLower(p)
		ok := true
		for _, w := range words {
			if !strings.Contains(lp, w) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		rank := len(p)
		base := strings.ToLower(pathBase(p))
		if strings.HasPrefix(base, words[0]) {
			rank -= 1000
		} else if strings.Contains(base, words[0]) {
			rank -= 500
		}
		hits = append(hits, hit{p, rank})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].rank < hits[j].rank })
	out := make([]string, 0, limit)
	for i := 0; i < len(hits) && i < limit; i++ {
		out = append(out, hits[i].path)
	}
	return out
}

func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a title into a file name: lower case, words joined with
// dashes, at most 40 characters, "thread" when nothing is left.
func Slug(title string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(title), "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		return "thread"
	}
	return s
}

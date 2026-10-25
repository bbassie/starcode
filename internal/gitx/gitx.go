// Package gitx reads git state by shelling out to git. It is read-only.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type File struct {
	Path   string
	Status string // two-letter porcelain code, e.g. " M", "??", "A "
}

type Status struct {
	IsRepo  bool
	Branch  string
	Files   []File
	Ahead   int
	Behind  int
	Summary string // "+12 -3" from diff --shortstat, empty when clean
}

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

// Read returns the working tree status for dir. A non-repo returns
// IsRepo=false and no error.
func Read(ctx context.Context, dir string) Status {
	var st Status
	if _, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return st
	}
	st.IsRepo = true
	if b, err := run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		st.Branch = strings.TrimSpace(b)
	}
	if out, err := run(ctx, dir, "status", "--porcelain", "-z"); err == nil {
		for _, rec := range strings.Split(out, "\x00") {
			if len(rec) < 4 {
				continue
			}
			st.Files = append(st.Files, File{Status: rec[:2], Path: rec[3:]})
		}
	}
	if out, err := run(ctx, dir, "diff", "--shortstat", "HEAD"); err == nil {
		st.Summary = shortstat(out)
	}
	return st
}

func shortstat(s string) string {
	// " 2 files changed, 12 insertions(+), 3 deletions(-)"
	var parts []string
	for _, p := range strings.Split(strings.TrimSpace(s), ",") {
		p = strings.TrimSpace(p)
		switch {
		case strings.Contains(p, "insertion"):
			parts = append(parts, "+"+strings.Fields(p)[0])
		case strings.Contains(p, "deletion"):
			parts = append(parts, "-"+strings.Fields(p)[0])
		}
	}
	return strings.Join(parts, " ")
}

// Diff returns the unified diff for one path (staged and unstaged combined
// against HEAD; untracked files are shown as an add).
func Diff(ctx context.Context, dir, path string) string {
	out, err := run(ctx, dir, "diff", "HEAD", "--", path)
	if err == nil && out != "" {
		return out
	}
	// Untracked: fake a diff against /dev/null so it renders the same way.
	out, err = run(ctx, dir, "diff", "--no-index", "--", "/dev/null", path)
	if out != "" {
		return out
	}
	if err != nil {
		return ""
	}
	return out
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

// DiffHit is one line of the working tree's changes that matches a
// search: the file, its line number (in the new file, or the old one for
// a removed line), the line's diff mark ('+', '-' or ' ') and its text.
type DiffHit struct {
	Path string
	Line int
	Op   byte
	Text string
}

// maxUntrackedRead caps how much of an untracked file FindInDiff reads.
const maxUntrackedRead = 1 << 20

// FindInDiff searches everything Diff would show for the changed files
// of dir, untracked ones included, for q. The search ignores case unless
// q has a capital letter. At most limit hits come back; more reports
// whether there were others.
func FindInDiff(ctx context.Context, dir, q string, limit int) (hits []DiffHit, more bool) {
	if q == "" {
		return nil, false
	}
	match := matcher(q)
	add := func(h DiffHit) bool {
		if !match(h.Text) {
			return true
		}
		if len(hits) == limit {
			more = true
			return false
		}
		hits = append(hits, h)
		return true
	}
	out, err := run(ctx, dir, "diff", "HEAD", "--no-color", "--no-ext-diff")
	if err == nil {
		for _, h := range parseDiff(out) {
			if !add(h) {
				return hits, more
			}
		}
	}
	untracked, err := run(ctx, dir, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return hits, more
	}
	for _, p := range strings.Split(untracked, "\x00") {
		if p == "" {
			continue
		}
		data, err := readHead(filepath.Join(dir, filepath.FromSlash(p)), maxUntrackedRead)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			if !add(DiffHit{Path: p, Line: i + 1, Op: '+', Text: line}) {
				return hits, more
			}
		}
	}
	return hits, more
}

// matcher is the test FindInDiff applies: smart case, as in most editors.
func matcher(q string) func(string) bool {
	if strings.ToLower(q) != q {
		return func(s string) bool { return strings.Contains(s, q) }
	}
	return func(s string) bool { return strings.Contains(strings.ToLower(s), q) }
}

// readHead reads at most n bytes of the file at path.
func readHead(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, n))
}

// hunkHead matches a unified diff hunk header: @@ -12,3 +14,5 @@.
var hunkHead = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// parseDiff reads the lines of a unified diff with their file and line
// numbers. Headers and binary notices are skipped.
func parseDiff(diff string) []DiffHit {
	var out []DiffHit
	var path string
	oldLine, newLine := 0, 0
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
			path = ""
		case !inHunk && strings.HasPrefix(line, "--- "):
			if p, ok := strings.CutPrefix(line[4:], "a/"); ok {
				path = p
			}
		case !inHunk && strings.HasPrefix(line, "+++ "):
			// A deleted file has +++ /dev/null and keeps the old name.
			if p, ok := strings.CutPrefix(line[4:], "b/"); ok {
				path = p
			}
		case strings.HasPrefix(line, "@@"):
			m := hunkHead.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			oldLine, _ = strconv.Atoi(m[1])
			newLine, _ = strconv.Atoi(m[2])
			inHunk = true
		case inHunk && line != "" && path != "":
			switch line[0] {
			case '+':
				out = append(out, DiffHit{Path: path, Line: newLine, Op: '+', Text: line[1:]})
				newLine++
			case '-':
				out = append(out, DiffHit{Path: path, Line: oldLine, Op: '-', Text: line[1:]})
				oldLine++
			case ' ':
				out = append(out, DiffHit{Path: path, Line: newLine, Op: ' ', Text: line[1:]})
				oldLine++
				newLine++
			}
		}
	}
	return out
}

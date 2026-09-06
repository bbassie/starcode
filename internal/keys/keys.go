// Package keys holds the keyboard shortcuts: a fixed list of actions with
// default combos, and the reader's overrides in <data>/keybindings.json.
// A combo is written as "mod+shift+k": modifiers in the order mod, alt,
// shift, then one key. "mod" is Ctrl, or Cmd on a Mac.
package keys

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Action is one thing a key can do. Where says which pages offer it, for
// the settings page; the views decide the actual set per page.
type Action struct {
	ID      string
	Label   string
	Desc    string
	Default string
	Where   string
}

// Actions is the full list, in the order the settings page shows them.
var Actions = []Action{
	{ID: "palette", Label: "Search and commands", Desc: "Open the palette", Default: "mod+k", Where: "everywhere"},
	{ID: "help", Label: "Keyboard shortcuts", Desc: "Show this list", Default: "mod+/", Where: "everywhere"},
	{ID: "sidebar", Label: "Toggle sidebar", Desc: "Hide or show the thread list", Default: "mod+b", Where: "everywhere"},
	{ID: "thread-prev", Label: "Previous thread", Desc: "Open the thread above the current one", Default: "alt+up", Where: "everywhere"},
	{ID: "thread-next", Label: "Next thread", Desc: "Open the thread below the current one", Default: "alt+down", Where: "everywhere"},
	{ID: "thread-new", Label: "New thread", Desc: "Start a thread in the current project", Default: "mod+shift+o", Where: "everywhere"},
	{ID: "settings", Label: "Settings", Desc: "Open the settings", Default: "mod+,", Where: "everywhere"},
	{ID: "terminal", Label: "Toggle terminal", Desc: "Open or close the shell panel", Default: "mod+`", Where: "threads, also inside the terminal"},
	{ID: "changes", Label: "Toggle changes panel", Desc: "Open or close the side panel", Default: "mod+shift+g", Where: "threads, also inside the terminal"},
	{ID: "prompt", Label: "Focus the prompt", Desc: "Put the cursor in the composer", Default: "mod+shift+f", Where: "threads, also inside the terminal"},
	{ID: "approve", Label: "Allow", Desc: "Answer the oldest waiting approval with allow", Default: "alt+y", Where: "threads with an approval waiting"},
	{ID: "approve-session", Label: "Allow for session", Desc: "Allow it and every later request like it in this thread", Default: "alt+shift+y", Where: "threads with an approval waiting"},
	{ID: "deny", Label: "Deny", Desc: "Answer the oldest waiting approval with deny", Default: "alt+n", Where: "threads with an approval waiting"},
}

// Binding is an action with the combo in force.
type Binding struct {
	Action
	Combo  string
	Custom bool // differs from the default
}

func action(id string) (Action, bool) {
	for _, a := range Actions {
		if a.ID == id {
			return a, true
		}
	}
	return Action{}, false
}

// Default is the built-in combo for id, or "" for an unknown id.
func Default(id string) string {
	a, _ := action(id)
	return a.Default
}

// Store is the overrides file. A nil *Store answers with the defaults.
type Store struct {
	path   string
	mu     sync.Mutex
	custom map[string]string
}

// Open reads path; a missing file means no overrides.
func Open(path string) (*Store, error) {
	s := &Store{path: path, custom: map[string]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.custom); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Anything the file says that no action or no key matches is dropped
	// rather than served.
	for id, c := range s.custom {
		n, err := Normalize(c)
		if _, ok := action(id); !ok || err != nil {
			delete(s.custom, id)
			continue
		}
		s.custom[id] = n
	}
	return s, nil
}

// Combo is the combo in force for id.
func (s *Store) Combo(id string) string {
	if s == nil {
		return Default(id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.custom[id]; ok {
		return c
	}
	return Default(id)
}

// All lists every action with its combo, in Actions order.
func (s *Store) All() []Binding {
	out := make([]Binding, 0, len(Actions))
	for _, a := range Actions {
		c := s.Combo(a.ID)
		out = append(out, Binding{Action: a, Combo: c, Custom: c != a.Default})
	}
	return out
}

// Set binds combo to id. A combo another action already has is refused,
// as is one the page needs for itself.
func (s *Store) Set(id, combo string) error {
	a, ok := action(id)
	if !ok {
		return fmt.Errorf("unknown action %q", id)
	}
	c, err := Normalize(combo)
	if err != nil {
		return err
	}
	for _, b := range s.All() {
		if b.ID != id && b.Combo == c {
			return fmt.Errorf("%s is already %s", Display(c), b.Label)
		}
	}
	s.mu.Lock()
	if c == a.Default {
		delete(s.custom, id)
	} else {
		s.custom[id] = c
	}
	err = s.save()
	s.mu.Unlock()
	return err
}

// Reset puts id back on its default; an empty id resets everything.
func (s *Store) Reset(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		s.custom = map[string]string{}
	} else {
		delete(s.custom, id)
	}
	return s.save()
}

func (s *Store) save() error {
	if len(s.custom) == 0 {
		err := os.Remove(s.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	b, err := json.MarshalIndent(s.custom, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

var namedKeys = map[string]bool{
	"up": true, "down": true, "left": true, "right": true, "enter": true, "escape": true, "space": true, "tab": true,
	"backspace": true, "delete": true, "home": true, "end": true, "pageup": true, "pagedown": true, "insert": true,
}

var fKey = regexp.MustCompile(`^f([1-9]|1[0-2])$`)

// Normalize checks a combo and writes it in canonical form: modifiers
// in the order mod, alt, shift, then the key in lower case.
func Normalize(combo string) (string, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(combo)), "+")
	if strings.HasSuffix(combo, "++") {
		// "mod++" is mod and the plus key.
		parts = append(parts[:len(parts)-2], "+")
	}
	var mod, alt, shift bool
	key := ""
	for _, p := range parts {
		switch p {
		case "mod", "ctrl", "control", "cmd", "meta":
			mod = true
		case "alt", "option":
			alt = true
		case "shift":
			shift = true
		case "":
			return "", errors.New("empty key")
		default:
			if key != "" {
				return "", fmt.Errorf("%q has two keys", combo)
			}
			key = p
		}
	}
	if key == "" {
		return "", errors.New("a shortcut needs a key besides the modifiers")
	}
	if len([]rune(key)) != 1 && !namedKeys[key] && !fKey.MatchString(key) {
		return "", fmt.Errorf("unknown key %q", key)
	}
	// Bare keys the page relies on: Escape closes things, Enter and Tab
	// belong to the focused control.
	if !mod && !alt && (key == "escape" || key == "enter" || key == "tab") {
		return "", fmt.Errorf("%s on its own is used by the page", Display(key))
	}
	var out []string
	if mod {
		out = append(out, "mod")
	}
	if alt {
		out = append(out, "alt")
	}
	if shift {
		out = append(out, "shift")
	}
	out = append(out, key)
	return strings.Join(out, "+"), nil
}

// Display writes a combo for a message: "Ctrl+Shift+K".
func Display(combo string) string {
	parts := strings.Split(combo, "+")
	if strings.HasSuffix(combo, "++") {
		parts = append(parts[:len(parts)-2], "+")
	}
	for i, p := range parts {
		switch p {
		case "mod":
			parts[i] = "Ctrl"
		default:
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "+")
}

// IDs lists the action ids, for callers that iterate.
func IDs() []string {
	out := make([]string, len(Actions))
	for i, a := range Actions {
		out[i] = a.ID
	}
	slices.Sort(out)
	return out
}

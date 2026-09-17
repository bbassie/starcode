package views

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every icon named by a literal in the templates should be in the sprite;
// an inline fallback still renders, but costs the bytes the sprite saves.
func TestSpriteCoversTemplateIcons(t *testing.T) {
	re := regexp.MustCompile(`Icon\("([a-z0-9-]+)"`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".templ") {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			if !spriteSet[m[1]] {
				t.Errorf("%s: icon %q is not in spriteIcons", e.Name(), m[1])
			}
		}
	}
}

func TestSpriteHasEverySymbol(t *testing.T) {
	body, version := Sprite()
	if version == "" {
		t.Fatal("empty sprite version")
	}
	for _, n := range spriteIcons {
		if !strings.Contains(body, `<symbol id="`+n+`"`) {
			t.Errorf("sprite lacks %q", n)
		}
	}
}

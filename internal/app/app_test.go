package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandProjectPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	got, err := expandProjectPath("~")
	if err != nil {
		t.Fatal(err)
	}
	if got != home {
		t.Errorf("expandProjectPath(~) = %q, want %q", got, home)
	}

	got, err = expandProjectPath("~/projects/starcode")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "projects", "starcode")
	if got != want {
		t.Errorf("expandProjectPath(~/...) = %q, want %q", got, want)
	}
}

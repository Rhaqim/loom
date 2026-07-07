package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfinedPromptFile(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "sys.txt")
	if err := os.WriteFile(inside, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A secret sibling outside the root that a traversal must not reach.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("disabled by default", func(t *testing.T) {
		e := &Engine{cfg: Config{}}
		if _, err := e.confinedPromptFile(inside); err == nil {
			t.Fatal("expected file-backed prompts to be disabled without PromptFileRoot")
		}
	})

	t.Run("allows path inside root", func(t *testing.T) {
		e := &Engine{cfg: Config{PromptFileRoot: root}}
		got, err := e.confinedPromptFile("sys.txt")
		if err != nil {
			t.Fatalf("expected in-root path to be allowed: %v", err)
		}
		if filepath.Base(got) != "sys.txt" {
			t.Fatalf("unexpected resolved path %q", got)
		}
	})

	t.Run("rejects traversal escape", func(t *testing.T) {
		e := &Engine{cfg: Config{PromptFileRoot: root}}
		if _, err := e.confinedPromptFile("../" + filepath.Base(filepath.Dir(outside)) + "/secret.txt"); err == nil {
			t.Fatal("expected ../ traversal to be rejected")
		}
	})

	t.Run("rejects absolute path outside root", func(t *testing.T) {
		e := &Engine{cfg: Config{PromptFileRoot: root}}
		if _, err := e.confinedPromptFile(outside); err == nil {
			t.Fatal("expected absolute out-of-root path to be rejected")
		}
	})
}

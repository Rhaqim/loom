package loom_test

// Guards llms.txt (the AI-oriented guide) against API drift: every `loom.<Name>`
// it references must still be a real exported symbol of the public facade. When
// this fails, a symbol in llms.txt was renamed or removed — fix the doc.
//
// Only top-level `loom.X` references are checked (types/funcs/consts/vars);
// method and field accesses (e.g. e.RunStep, req.Overrides) and subpackage
// references (openai.NewChatGenerator) are out of scope.

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

var loomRefRe = regexp.MustCompile(`\bloom\.([A-Z][A-Za-z0-9]*)`)

func TestLLMSTxtReferencesExist(t *testing.T) {
	data, err := os.ReadFile("llms.txt")
	if err != nil {
		t.Fatal(err)
	}
	valid := exportedTopLevel(t, ".") // exported top-level symbols of the facade

	seen := map[string]bool{}
	var unknown []string
	for _, m := range loomRefRe.FindAllStringSubmatch(string(data), -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if !valid[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Fatalf("llms.txt references %d loom.* symbol(s) not in the public API (renamed/removed — update the doc): %v",
			len(unknown), unknown)
	}
}

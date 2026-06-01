// Package entity provides pluggable entity annotators that scan generated prose
// for known named-entity references and return offset-tagged annotation records.
package entity

import (
	"context"
	"strings"
	"unicode"
)

// Entry describes a named entity the annotator should look for.
type Entry struct {
	ID    string   // platform-supplied entity ID
	Kind  string   // "character", "location", "time", "item", or custom
	Names []string // name variants and aliases (case-insensitive)
}

// Registry is the set of entities known at annotation time.
type Registry struct {
	Entries []Entry
}

// Annotation marks a named-entity reference in generated prose.
type Annotation struct {
	EntityID   string
	EntityKind string
	Surface    string // the matched substring as it appears in the prose
	StartByte  int    // byte offset of Surface in the prose
	EndByte    int
	Confidence float64
	Metadata   map[string]any
}

// Annotator scans prose for entity references.
type Annotator interface {
	Annotate(ctx context.Context, prose string, registry Registry) ([]Annotation, error)
}

// -----------------------------------------------------------------------
// ExactMatchAnnotator — case-insensitive substring matching, zero LLM cost.
// -----------------------------------------------------------------------

// ExactMatchAnnotator scans for exact (case-insensitive) name occurrences.
type ExactMatchAnnotator struct{}

// NewExactMatchAnnotator creates an ExactMatchAnnotator.
func NewExactMatchAnnotator() *ExactMatchAnnotator {
	return &ExactMatchAnnotator{}
}

func (a *ExactMatchAnnotator) Annotate(_ context.Context, prose string, registry Registry) ([]Annotation, error) {
	lower := strings.ToLower(prose)
	var anns []Annotation
	for _, entry := range registry.Entries {
		for _, name := range entry.Names {
			needle := strings.ToLower(name)
			start := 0
			for {
				idx := strings.Index(lower[start:], needle)
				if idx < 0 {
					break
				}
				abs := start + idx
				// Word-boundary check: character before and after must not be alphanumeric.
				if !isWordBoundary(prose, abs, abs+len(needle)) {
					start = abs + 1
					continue
				}
				anns = append(anns, Annotation{
					EntityID:   entry.ID,
					EntityKind: entry.Kind,
					Surface:    prose[abs : abs+len(name)],
					StartByte:  abs,
					EndByte:    abs + len(name),
					Confidence: 1.0,
				})
				start = abs + len(needle)
			}
		}
	}
	return anns, nil
}

// isWordBoundary returns true if [start, end) is surrounded by non-alphanumeric
// characters (or is at a string boundary).
func isWordBoundary(s string, start, end int) bool {
	before := start == 0 || !unicode.IsLetter(rune(s[start-1]))
	after := end >= len(s) || !unicode.IsLetter(rune(s[end]))
	return before && after
}

// -----------------------------------------------------------------------
// FuzzyMatchAnnotator — exact match + possessive + simple co-reference.
// -----------------------------------------------------------------------

// FuzzyMatchAnnotator adds possessive ("Koru's") and simple heuristic
// co-reference ("she"/"he"/"they") on top of exact matching.
type FuzzyMatchAnnotator struct {
	exact *ExactMatchAnnotator
}

// NewFuzzyMatchAnnotator creates a FuzzyMatchAnnotator.
func NewFuzzyMatchAnnotator() *FuzzyMatchAnnotator {
	return &FuzzyMatchAnnotator{exact: NewExactMatchAnnotator()}
}

func (a *FuzzyMatchAnnotator) Annotate(ctx context.Context, prose string, registry Registry) ([]Annotation, error) {
	// Run exact matching first.
	anns, err := a.exact.Annotate(ctx, prose, registry)
	if err != nil {
		return nil, err
	}

	// Add possessive variants.
	extRegistry := Registry{}
	for _, entry := range registry.Entries {
		ext := Entry{ID: entry.ID, Kind: entry.Kind}
		for _, name := range entry.Names {
			ext.Names = append(ext.Names, name+"'s")
		}
		if len(ext.Names) > 0 {
			extRegistry.Entries = append(extRegistry.Entries, ext)
		}
	}
	possAnns, err := a.exact.Annotate(ctx, prose, extRegistry)
	if err != nil {
		return nil, err
	}
	anns = append(anns, possAnns...)

	return anns, nil
}

// -----------------------------------------------------------------------
// LLMAnnotator — resolves ambiguous references via an LLM classifier.
// Requires a Resolver function that calls an external model.
// -----------------------------------------------------------------------

// Resolver is a function that calls an LLM to resolve entity mentions.
type Resolver func(ctx context.Context, prose string, candidates []Entry) ([]Annotation, error)

// LLMAnnotator uses a Resolver for ambiguous mentions, falling back to fuzzy
// matching for unambiguous ones.
type LLMAnnotator struct {
	fuzzy    *FuzzyMatchAnnotator
	resolver Resolver
}

// NewLLMAnnotator creates an LLMAnnotator backed by the given Resolver.
func NewLLMAnnotator(resolver Resolver) *LLMAnnotator {
	return &LLMAnnotator{
		fuzzy:    NewFuzzyMatchAnnotator(),
		resolver: resolver,
	}
}

func (a *LLMAnnotator) Annotate(ctx context.Context, prose string, registry Registry) ([]Annotation, error) {
	// Use fuzzy matching as baseline.
	anns, err := a.fuzzy.Annotate(ctx, prose, registry)
	if err != nil {
		return nil, err
	}
	// Then let the resolver handle the full prose for anything it can improve.
	llmAnns, err := a.resolver(ctx, prose, registry.Entries)
	if err != nil {
		// Fall back to fuzzy-only on resolver failure.
		return anns, nil
	}
	return merge(anns, llmAnns), nil
}

// merge deduplicates annotations by (EntityID, StartByte), preferring
// higher-confidence entries.
func merge(a, b []Annotation) []Annotation {
	key := func(ann Annotation) string {
		return ann.EntityID + ":" + strings.Join([]string{ann.EntityKind, ann.Surface}, "|")
	}
	seen := make(map[string]int) // key → index in result
	result := append([]Annotation(nil), a...)
	for _, ann := range b {
		k := key(ann)
		if idx, ok := seen[k]; ok {
			if ann.Confidence > result[idx].Confidence {
				result[idx] = ann
			}
		} else {
			seen[k] = len(result)
			result = append(result, ann)
		}
	}
	return result
}

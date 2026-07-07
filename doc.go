// Package loom is a modality-agnostic procgen engine for building AI-driven
// interactive narratives, games, and generative platforms. It provides session
// management, versioned agents and prompts, a hook bus for pre/post processors,
// cost tracking with budget enforcement, branch/replay support, an entity
// annotator, a judge subsystem, and a first-class test harness.
//
// This package is the stable public API. It is a thin facade: the types and
// constructors below are re-exported (via type aliases and value re-exports)
// from the internal implementation in internal/engine, which external modules
// cannot import. Depend on this package; the internal layout may change without
// notice. See aliases.go for the generated re-export surface.
//
//go:generate go run ./internal/tools/facadegen internal/engine aliases.go
package loom

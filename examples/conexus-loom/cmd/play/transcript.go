package main

// transcript.go is a self-contained, optional component: it appends an
// investor-friendly Markdown record of the playthrough (prose, choices, and a
// per-step spend report). To disable, run with -transcript "" or delete the
// three wiring lines in main.go marked "transcript". Every method is nil-safe.

import (
	"fmt"
	"os"
	"strings"

	"github.com/rhaqim/conexus-refactor/domain"
	"github.com/rhaqim/conexus-refactor/engine"
)

type Transcript struct {
	f *os.File
}

// NewTranscript opens path for appending. An empty path returns a nil Transcript
// (all methods become no-ops).
func NewTranscript(path string) (*Transcript, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Transcript{f: f}, nil
}

func (t *Transcript) Header(w domain.World, promptVersion int, resumed bool) {
	if t == nil {
		return
	}
	if resumed {
		fmt.Fprintf(t.f, "\n---\n\n_(resumed)_\n\n")
		return
	}
	fmt.Fprintf(t.f, "# %s\n\n_%s · generated playthrough · prompt v%d_\n\n> %s\n\n",
		w.Name, w.Genre, promptVersion, w.Premise)
}

// Turn records one beat: the choice that led to it, the prose, the next options,
// the art/sound direction, and the spend.
func (t *Transcript) Turn(action string, tr *engine.TurnResult, runTotalUSD float64) {
	if t == nil {
		return
	}
	fmt.Fprintf(t.f, "## Step %d\n\n", tr.Step)
	if action != "" {
		fmt.Fprintf(t.f, "_› chose: %s_\n\n", action)
	}
	fmt.Fprintf(t.f, "%s\n\n", tr.Prose)
	if tr.Sensory != nil {
		fmt.Fprintf(t.f, "`art: %s` · `music: %s`\n\n", tr.Sensory.ImagePrompt, tr.Sensory.MusicMood)
	}
	if len(tr.Options) > 0 {
		var b strings.Builder
		for i, o := range tr.Options {
			fmt.Fprintf(&b, "%d. **%s** — %s\n", i+1, o.Label, o.Detail)
		}
		fmt.Fprintf(t.f, "%s\n", b.String())
	}
	fmt.Fprintf(t.f, "`spend: %d→%d tokens · ~$%.4f this step · ~$%.4f total`%s\n\n",
		tr.TokensIn, tr.TokensOut, tr.USD, runTotalUSD, retried(tr.QARetried))
}

func (t *Transcript) End(status string, steps int, totalUSD float64) {
	if t == nil {
		return
	}
	fmt.Fprintf(t.f, "---\n\n## The End — %s\n\n**%d steps · ~$%.4f total spend**\n\n", status, steps, totalUSD)
}

func (t *Transcript) Path() string {
	if t == nil {
		return ""
	}
	return t.f.Name()
}

func (t *Transcript) Close() {
	if t == nil {
		return
	}
	_ = t.f.Close()
}

package loom

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"text/template/parse"

	"github.com/google/uuid"
)

type templateSession struct {
	ID         uuid.UUID
	PlatformID string
	Tags       []string
	State      State
}

const (
	// maxTemplateBodyBytes bounds the size of a user-template body we will parse.
	maxTemplateBodyBytes = 1 << 20 // 1 MiB
	// maxRenderedBytes bounds the size of a rendered prompt. A template that
	// expands without bound (e.g. a large {{range}}) is cut off here instead of
	// exhausting memory.
	maxRenderedBytes = 4 << 20 // 4 MiB
)

// errRenderTooLarge is the sentinel written by limitedWriter when the rendered
// output exceeds maxRenderedBytes.
var errRenderTooLarge = errors.New("loom: rendered template exceeds size limit")

// limitedWriter caps how many bytes may be written, failing the render instead
// of allowing an unbounded template expansion to exhaust memory.
type limitedWriter struct {
	buf *bytes.Buffer
	n   int
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.n+len(p) > w.max {
		return 0, errRenderTooLarge
	}
	w.n += len(p)
	return w.buf.Write(p)
}

// renderTemplate renders a Go text/template with a minimal session view, the
// action, and per-step inputs as data. The body size, the rendered output size,
// and any panic during execution are all bounded: a hostile or malformed
// template (e.g. one that recurses via {{template}} into a stack overflow, or
// expands without bound) degrades to an error rather than crashing the process.
func renderTemplate(body string, session *Session, action *Action, inputs, params map[string]any) (out string, err error) {
	if len(body) > maxTemplateBodyBytes {
		return "", fmt.Errorf("loom: template body exceeds %d bytes", maxTemplateBodyBytes)
	}
	// text/template executes with the caller's stack; a self-recursive template
	// definition can overflow it. Recover so one bad template cannot take down
	// the engine and every in-flight session with it.
	defer func() {
		if r := recover(); r != nil {
			out = ""
			err = fmt.Errorf("loom: template render panicked: %v", r)
		}
	}()

	tmpl, err := template.New("user").Parse(body)
	if err != nil {
		return "", err
	}
	// {{template}} invocations (and {{define}}/{{block}}) enable unbounded
	// recursion, which produces a fatal, unrecoverable Go stack overflow that no
	// recover() or output cap can stop. No first-party template needs them, so
	// reject them outright.
	if len(tmpl.Templates()) > 1 || treeHasTemplateInvocation(tmpl.Tree) {
		return "", fmt.Errorf("loom: nested template definitions/invocations are not allowed")
	}
	data := map[string]any{
		"Session": templateSession{
			ID:         session.ID,
			PlatformID: session.PlatformID,
			Tags:       session.Tags,
			State:      session.State,
		},
		"State":    session.State,
		"Snapshot": session.State.Snapshot,
		"Vars":     session.State.Vars,
		"Action":   action,
		"Inputs":   inputs,
		"Params":   params,
	}
	var buf bytes.Buffer
	lw := &limitedWriter{buf: &buf, max: maxRenderedBytes}
	if err := tmpl.Execute(lw, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// treeHasTemplateInvocation reports whether the parse tree contains a
// {{template}} action, which (via self- or mutual-invocation) is the vector for
// unbounded template recursion.
func treeHasTemplateInvocation(t *parse.Tree) bool {
	if t == nil || t.Root == nil {
		return false
	}
	return nodeHasTemplateInvocation(t.Root)
}

func nodeHasTemplateInvocation(n parse.Node) bool {
	switch v := n.(type) {
	case *parse.ListNode:
		if v == nil {
			return false
		}
		for _, c := range v.Nodes {
			if nodeHasTemplateInvocation(c) {
				return true
			}
		}
	case *parse.TemplateNode:
		return true
	case *parse.IfNode:
		return nodeHasTemplateInvocation(v.List) || nodeHasTemplateInvocation(v.ElseList)
	case *parse.RangeNode:
		return nodeHasTemplateInvocation(v.List) || nodeHasTemplateInvocation(v.ElseList)
	case *parse.WithNode:
		return nodeHasTemplateInvocation(v.List) || nodeHasTemplateInvocation(v.ElseList)
	}
	return false
}

// collectResultContext returns the last N results from step history for context continuity.
func collectResultContext(history []Step) []Result {
	const window = 10
	start := 0
	if len(history) > window {
		start = len(history) - window
	}
	out := make([]Result, 0, len(history)-start)
	for _, s := range history[start:] {
		if s.Result != nil {
			out = append(out, s.Result)
		}
	}
	return out
}

func buildStoredRequest(req GenerateRequest) StoredRequest {
	return StoredRequest{
		SystemPrompt:   req.SystemPrompt,
		UserPrompt:     req.UserPrompt,
		Params:         req.Params,
		ResponseFormat: req.ResponseFormat,
	}
}

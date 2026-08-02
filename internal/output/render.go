package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Table is the row-oriented view of a result, used by the pretty and TSV
// renderers. Commands build it alongside the JSON payload rather than letting
// each renderer reach into the data itself.
type Table struct {
	Headers []string
	Rows    [][]string
}

// Empty reports whether the table has nothing to print.
func (t Table) Empty() bool { return len(t.Headers) == 0 && len(t.Rows) == 0 }

// Payload is what a command hands to a renderer: Data is serialised by the JSON
// renderer, Table is printed by the pretty and TSV renderers, JUnit is written
// by the JUnit renderer. A command fills in whichever the formats it supports
// need.
type Payload struct {
	Data  any
	Table Table
	JUnit Suite
}

// Renderer writes a payload in one output format.
type Renderer interface {
	Render(p Payload) error
}

// New returns the renderer for f, writing to w.
func New(f Format, w io.Writer) Renderer {
	switch f {
	case FormatPretty:
		return prettyRenderer{w: w}
	case FormatTSV:
		return tsvRenderer{w: w}
	case FormatJUnit:
		return junitRenderer{w: w}
	default:
		return jsonRenderer{w: w}
	}
}

type jsonRenderer struct{ w io.Writer }

func (r jsonRenderer) Render(p Payload) error {
	body, err := marshalEnvelope(p.Data)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(r.w, "%s\n", body)
	return err
}

// marshalEnvelope serialises data with the versioned schema field as the first
// top-level key. Object payloads keep their fields at the top level, matching
// the shape in DESIGN.md §3.1; anything else (a bare list, a scalar) has no room
// for a sibling field and is nested under "data" instead.
//
// The output is deterministic: encoding/json sorts map keys, and the schema
// field is spliced in at a fixed position rather than merged through a map.
func marshalEnvelope(data any) ([]byte, error) {
	// SchemaVersion is a compile-time constant with no characters needing JSON
	// escaping, so it can be quoted directly.
	prefix := []byte(`{"schema":"` + SchemaVersion + `"`)

	if data == nil {
		return append(prefix, '}'), nil
	}

	body, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("rendering json output: %w", err)
	}

	if len(body) == 0 || body[0] != '{' {
		return json.Marshal(struct {
			Schema string `json:"schema"`
			Data   any    `json:"data"`
		}{SchemaVersion, data})
	}

	// body[0] is '{'; drop it and splice the rest onto the prefix. json.Marshal
	// emits no insignificant whitespace, so "{}" is the only empty-object form.
	if string(body) == "{}" {
		return append(prefix, '}'), nil
	}
	out := append(prefix, ',')
	return append(out, body[1:]...), nil
}

type tsvRenderer struct{ w io.Writer }

// Render writes one tab-separated line per row. Headers are deliberately
// omitted: TSV exists to be cut, awk'd and piped, and a header line would have
// to be skipped by every consumer. Use --output json for labelled fields.
func (r tsvRenderer) Render(p Payload) error {
	var b strings.Builder
	for _, row := range p.Table.Rows {
		b.WriteString(strings.Join(row, "\t"))
		b.WriteByte('\n')
	}

	_, err := io.WriteString(r.w, b.String())
	return err
}

type prettyRenderer struct{ w io.Writer }

// Render prints a column-aligned table. A payload with no table has no human
// shape to print, so it falls back to JSON rather than emitting nothing.
func (r prettyRenderer) Render(p Payload) error {
	if p.Table.Empty() {
		return jsonRenderer{w: r.w}.Render(p)
	}

	tw := tabwriter.NewWriter(r.w, 0, 0, 2, ' ', 0)
	if len(p.Table.Headers) > 0 {
		fmt.Fprintln(tw, strings.Join(p.Table.Headers, "\t"))
	}
	for _, row := range p.Table.Rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

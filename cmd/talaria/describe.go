package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Teeeep/talaria/internal/operation"
	"github.com/Teeeep/talaria/internal/output"
)

// describeView is the JSON payload: everything an agent needs to build a call
// to this operation, and nothing it does not.
type describeView struct {
	ID          string             `json:"id"`
	Method      string             `json:"method"`
	Path        string             `json:"path"`
	Summary     string             `json:"summary,omitempty"`
	Description string             `json:"description,omitempty"`
	Tags        []string           `json:"tags"`
	Deprecated  bool               `json:"deprecated,omitempty"`
	Params      []describeParam    `json:"params"`
	RequestBody *describeBody      `json:"request_body,omitempty"`
	Responses   []describeResponse `json:"responses"`
}

type describeParam struct {
	Name        string             `json:"name"`
	In          string             `json:"in"`
	Required    bool               `json:"required"`
	Description string             `json:"description,omitempty"`
	Schema      *output.SchemaNode `json:"schema,omitempty"`
}

type describeBody struct {
	Required    bool              `json:"required"`
	Description string            `json:"description,omitempty"`
	Content     []describeContent `json:"content"`
}

type describeContent struct {
	ContentType string             `json:"content_type"`
	Schema      *output.SchemaNode `json:"schema,omitempty"`
}

type describeResponse struct {
	Status      string            `json:"status"`
	Description string            `json:"description,omitempty"`
	Content     []describeContent `json:"content"`
}

func newDescribeCmd() *cobra.Command {
	var depth int

	cmd := &cobra.Command{
		Use:   "describe [spec] <operationId>",
		Short: "Show one operation's parameters, body and responses",
		Long: "Describe a single operation: its parameters, its request body and its\n" +
			"declared responses, with every schema rendered as one compact line per\n" +
			"field rather than as raw JSON Schema.",
		Args: usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := resolveFormat(cmd)
			if err != nil {
				return err
			}

			// The operationId is always last: with one argument the spec comes
			// from --spec or the environment, with two it is the first.
			id := args[len(args)-1]

			index, err := loadIndex(cmd, args[:len(args)-1])
			if err != nil {
				return err
			}

			op, err := index.Lookup(id)
			if err != nil {
				return err
			}

			return output.New(format, cmd.OutOrStdout()).Render(describePayload(op, depth))
		},
	}

	cmd.Flags().IntVar(&depth, "depth", output.DefaultSchemaDepth,
		"how many levels of nested schema to render before truncating")

	return cmd
}

// describePayload builds the JSON view and the rendered lines from the same
// walk of the operation, so the two views cannot disagree about what the
// operation takes.
func describePayload(op operation.Operation, depth int) output.Payload {
	view := describeView{
		ID:          op.ID,
		Method:      op.Method,
		Path:        op.Path,
		Summary:     op.Summary,
		Description: op.Description,
		Tags:        op.Tags,
		Deprecated:  op.Deprecated,
		Params:      make([]describeParam, 0, len(op.Params)),
		Responses:   make([]describeResponse, 0, len(op.Responses)),
	}

	for _, p := range op.Params {
		view.Params = append(view.Params, describeParam{
			Name:        p.Name,
			In:          p.In,
			Required:    p.Required,
			Description: p.Description,
			Schema:      paramSchema(p, depth),
		})
	}
	if op.RequestBody != nil {
		view.RequestBody = &describeBody{
			Required:    op.RequestBody.Required,
			Description: op.RequestBody.Description,
			Content:     describeContents(op.RequestBody.Content, depth),
		}
	}
	for _, r := range op.Responses {
		view.Responses = append(view.Responses, describeResponse{
			Status:      r.Status,
			Description: r.Description,
			Content:     describeContents(r.Content, depth),
		})
	}
	if view.Tags == nil {
		// Same contract as list: a field that changes type between specs is a
		// trap for anything parsing this.
		view.Tags = []string{}
	}

	return output.Payload{Data: view, Table: output.Table{Rows: describeRows(view)}}
}

// paramSchema renders a parameter's schema as a named node, so it prints in the
// same `name*: (type) description` shape as a body field. A parameter with no
// schema — the rare content-typed form — still gets a line, typed "any".
func paramSchema(p operation.Param, depth int) *output.SchemaNode {
	node := output.SchemaTree(p.Schema, depth)
	node.Name = p.Name
	node.Required = p.Required
	if node.Type == "" {
		node.Type = typeAny
	}

	// The parameter's own description wins over its schema's: the schema is
	// often a shared $ref described in general terms, while the parameter
	// describes this use of it.
	if p.Description != "" {
		node.Description = strings.Join(strings.Fields(p.Description), " ")
	}

	return &node
}

// typeAny is the type shown for a parameter the spec gave no schema at all.
const typeAny = "any"

// paramLine renders a parameter as one line, with its location leading the
// description: `in` is what distinguishes two same-named parameters, and a
// reader deciding how to pass one needs it before the prose. The location stays
// out of the node itself so `--output json` reports it once, as a field.
func paramLine(p describeParam) string {
	node := *p.Schema
	node.Description = strings.TrimSpace("[" + p.In + "] " + node.Description)

	return node.Line()
}

func describeContents(content []operation.MediaType, depth int) []describeContent {
	out := make([]describeContent, 0, len(content))
	for _, m := range content {
		entry := describeContent{ContentType: m.ContentType}
		if node := output.SchemaTree(m.Schema, depth); node.Type != "" {
			entry.Schema = &node
		}
		out = append(out, entry)
	}

	return out
}

// describeRows renders the view as single-cell rows. describe is prose with
// structure, not a table, so each row is one whole line; the tabwriter behind
// the pretty renderer passes a single-cell row through untouched.
func describeRows(view describeView) [][]string {
	var lines []string
	add := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	section := func(heading string) {
		add("")
		add("%s", heading)
	}

	header := fmt.Sprintf("%s %s  %s", view.Method, view.Path, view.ID)
	if view.Deprecated {
		header += "  [deprecated]"
	}
	add("%s", header)
	if view.Summary != "" {
		add("%s", view.Summary)
	}
	if view.Description != "" && view.Description != view.Summary {
		add("%s", view.Description)
	}
	if len(view.Tags) > 0 {
		add("tags: %s", strings.Join(view.Tags, ", "))
	}

	if len(view.Params) > 0 {
		section("Params:")
		for _, p := range view.Params {
			add("%s%s", indent, paramLine(p))
			addNested(&lines, p.Schema, 2)
		}
	}

	if view.RequestBody != nil {
		section("Body:")
		for _, c := range view.RequestBody.Content {
			add("%s%s%s", indent, c.ContentType, requiredSuffix(view.RequestBody.Required))
			addSchema(&lines, c.Schema, 2)
		}
	}

	if len(view.Responses) > 0 {
		section("Responses:")
		for _, r := range view.Responses {
			add("%s%s", indent, strings.TrimSpace(r.Status+" "+r.Description))
			for _, c := range r.Content {
				add("%s%s", strings.Repeat(indent, 2), c.ContentType+":")
				addSchema(&lines, c.Schema, 3)
			}
		}
	}

	rows := make([][]string, 0, len(lines))
	for _, line := range lines {
		rows = append(rows, []string{line})
	}

	return rows
}

// indent is one nesting level of the pretty layout, matching the schema
// renderer's own.
const indent = "  "

// addSchema appends a schema block indented to depth levels. A content type
// with no schema contributes nothing rather than an empty block.
func addSchema(lines *[]string, node *output.SchemaNode, depth int) {
	if node == nil {
		return
	}
	for _, line := range node.Lines() {
		*lines = append(*lines, strings.Repeat(indent, depth)+line)
	}
}

// addNested appends only a node's children, for the case where the node's own
// line has already been printed — a parameter whose schema is an object.
func addNested(lines *[]string, node *output.SchemaNode, depth int) {
	if node == nil {
		return
	}
	for _, child := range node.Properties {
		for _, line := range child.Lines() {
			*lines = append(*lines, strings.Repeat(indent, depth)+line)
		}
	}
}

func requiredSuffix(required bool) string {
	if required {
		return " (required)"
	}

	return " (optional)"
}

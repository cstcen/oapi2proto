package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Simplified OAS structures (minimal fields used)
type Document struct {
	Components struct {
		Schemas map[string]*Schema `json:"schemas" yaml:"schemas"`
	} `json:"components" yaml:"components"`
}

type Schema struct {
	Ref         string             `json:"$ref" yaml:"$ref"`
	Type        string             `json:"type" yaml:"type"`
	Format      string             `json:"format" yaml:"format"`
	Enum        []string           `json:"enum" yaml:"enum"`
	Properties  map[string]*Schema `json:"properties" yaml:"properties"`
	Items       *Schema            `json:"items" yaml:"items"`
	OneOf       []*Schema          `json:"oneOf" yaml:"oneOf"`
	AllOf       []*Schema          `json:"allOf" yaml:"allOf"`
	AnyOf       []*Schema          `json:"anyOf" yaml:"anyOf"`
	Required    []string           `json:"required" yaml:"required"`
	Nullable    bool               `json:"nullable" yaml:"nullable"`
	AddlProps   *Schema            `json:"additionalProperties" yaml:"additionalProperties"`
	Description string             `json:"description" yaml:"description"`
}

func main() {
	in := flag.String("in", "openapi.json", "openapi v3 file (json or yaml)")
	out := flag.String("out", "api.proto", "output proto file path")
	pkg := flag.String("pkg", "api.v1", "proto package")
	goPkg := flag.String("go_pkg", "example.com/project/api/v1;v1", "go_package option value")
	useOptional := flag.Bool("use-optional", true, "emit 'optional' for nullable scalars")
	anyOfMode := flag.String("anyof", "oneof", "anyof handling: oneof|repeat")
	sortFields := flag.Bool("sort", true, "sort fields & schemas alphabetically for stability")
	flag.Parse()

	data, err := os.ReadFile(*in)
	if err != nil {
		fatal(err)
	}

	var doc Document
	var jsonErr error
	if jErr := json.Unmarshal(data, &doc); jErr != nil || len(doc.Components.Schemas) == 0 {
		jsonErr = jErr
		// attempt YAML
		var ydoc Document
		yErr := yaml.Unmarshal(data, &ydoc)
		if yErr == nil && len(ydoc.Components.Schemas) > 0 {
			doc = ydoc
		} else if jsonErr != nil { // both failed or empty
			fatal(fmt.Errorf("parse openapi (json/yaml) failed: jsonErr=%v yamlErr=%v", jsonErr, yErr))
		}
	}

	if len(doc.Components.Schemas) == 0 {
		fatal(errors.New("no components.schemas found"))
	}

	var b strings.Builder
	b.WriteString("syntax = \"proto3\";\n")
	b.WriteString(fmt.Sprintf("package %s;\n", *pkg))
	b.WriteString(fmt.Sprintf("option go_package = \"%s\";\n\n", *goPkg))

	// Collect schema names
	names := make([]string, 0, len(doc.Components.Schemas))
	for name := range doc.Components.Schemas {
		names = append(names, name)
	}
	if *sortFields {
		sort.Strings(names)
	}

	ctx := &genContext{doc: &doc, useOptional: *useOptional, anyOfMode: *anyOfMode, sortFields: *sortFields, visited: map[string]bool{}}

	for _, name := range names {
		ctx.emitSchema(&b, name, doc.Components.Schemas[name])
	}

	if err := os.MkdirAll(path.Dir(*out), 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
		fatal(err)
	}
}

type genContext struct {
	doc         *Document
	useOptional bool
	anyOfMode   string
	sortFields  bool
	visited     map[string]bool
}

func (g *genContext) emitSchema(b *strings.Builder, name string, s *Schema) {
	if g.visited[name] {
		return
	}
	g.visited[name] = true
	resolved := g.resolveRef(s)
	if len(resolved.Enum) > 0 {
		g.emitEnum(b, name, resolved)
		return
	}
	if resolved.Type == "object" || resolved.Properties != nil || resolved.AllOf != nil || resolved.OneOf != nil || resolved.AnyOf != nil || resolved.AddlProps != nil {
		g.emitMessage(b, name, resolved)
		return
	}
	// Primitive at top-level: wrap in message
	b.WriteString(fmt.Sprintf("// Primitive schema %s promoted to wrapper message\n", name))
	b.WriteString(fmt.Sprintf("message %s { %s value = 1; }\n\n", normalizeMessage(name), g.scalarType(resolved)))
}

func (g *genContext) emitEnum(b *strings.Builder, name string, s *Schema) {
	enumName := normalizeMessage(name)
	b.WriteString(fmt.Sprintf("enum %s {\n", enumName))
	b.WriteString(fmt.Sprintf("  %s_UNSPECIFIED = 0;\n", strings.ToUpper(enumName)))
	for i, v := range s.Enum {
		b.WriteString(fmt.Sprintf("  %s_%s = %d;\n", strings.ToUpper(enumName), toEnumValue(v), i+1))
	}
	b.WriteString("}\n\n")
}

func (g *genContext) emitMessage(b *strings.Builder, name string, s *Schema) {
	msgName := normalizeMessage(name)
	b.WriteString(fmt.Sprintf("message %s {\n", msgName))
	// Merge allOf properties first
	merged := &Schema{Properties: map[string]*Schema{}, Required: s.Required}
	// allOf merge
	for _, part := range s.AllOf {
		merged = mergeInto(merged, g.resolveRef(part))
	}
	// base properties
	for k, v := range s.Properties {
		merged.Properties[k] = v
	}

	// Track field numbers
	fieldNum := 1
	propNames := make([]string, 0, len(merged.Properties))
	for k := range merged.Properties {
		propNames = append(propNames, k)
	}
	if g.sortFields {
		sort.Strings(propNames)
	}
	// Collect nested schemas to emit later (flatten)
	type pending struct {
		name   string
		schema *Schema
	}
	var toEmit []pending
	for _, prop := range propNames {
		ps := merged.Properties[prop]
		ptype, nested := g.fieldType(prop, ps)
		if nested != nil { // defer emission for flatten, rename with parent prefix
			baseNestedName := nested[0].(string)
			flatName := normalizeMessage(msgName + "_" + baseNestedName)
			// replace ptype with flattened name
			ptype = flatName
			// schedule emission if not visited yet under new name
			if !g.visited[flatName] {
				toEmit = append(toEmit, pending{name: flatName, schema: nested[1].(*Schema)})
			}
		}
		opt := ""
		if ps.Nullable && g.useOptional && isScalar(ptype) {
			opt = "optional "
		}
		b.WriteString(fmt.Sprintf("  %s%s %s = %d;", opt, ptype, normalizeField(prop), fieldNum))
		if ps.Description != "" {
			b.WriteString(fmt.Sprintf(" // %s", oneline(ps.Description)))
		}
		b.WriteString("\n")
		fieldNum++
	}

	// map type
	if s.AddlProps != nil && len(merged.Properties) == 0 {
		valType, nested := g.fieldType("value", s.AddlProps)
		if nested != nil {
			g.emitSchema(b, nested[0].(string), nested[1].(*Schema))
		}
		b.WriteString(fmt.Sprintf("  map<string,%s> entries = 1;\n", valType))
	}

	// oneOf -> oneof block
	if len(s.OneOf) > 0 {
		b.WriteString("  oneof one_of {\n")
		idx := 0
		for _, branch := range s.OneOf {
			idx++
			pt, nested := g.fieldType(fmt.Sprintf("choice_%d", idx), branch)
			if nested != nil {
				baseNestedName := nested[0].(string)
				flatName := normalizeMessage(msgName + "_" + baseNestedName)
				pt = flatName
				if !g.visited[flatName] {
					toEmit = append(toEmit, pending{name: flatName, schema: nested[1].(*Schema)})
				}
			}
			b.WriteString(fmt.Sprintf("    %s %s = %d;\n", pt, fmt.Sprintf("choice_%d", idx), fieldNum))
			fieldNum++
		}
		b.WriteString("  }\n")
	}
	// anyOf handling
	if len(s.AnyOf) > 0 {
		if g.anyOfMode == "repeat" {
			pt, nested := g.fieldType("anyof_value", s.AnyOf[0])
			if nested != nil {
				baseNestedName := nested[0].(string)
				flatName := normalizeMessage(msgName + "_" + baseNestedName)
				pt = flatName
				if !g.visited[flatName] {
					toEmit = append(toEmit, pending{name: flatName, schema: nested[1].(*Schema)})
				}
			}
			b.WriteString(fmt.Sprintf("  repeated %s anyof_value = %d; // anyOf first schema repeated\n", pt, fieldNum))
			fieldNum++
		} else {
			b.WriteString("  oneof any_of {\n")
			idx := 0
			for _, branch := range s.AnyOf {
				idx++
				pt, nested := g.fieldType(fmt.Sprintf("alt_%d", idx), branch)
				if nested != nil {
					baseNestedName := nested[0].(string)
					flatName := normalizeMessage(msgName + "_" + baseNestedName)
					pt = flatName
					if !g.visited[flatName] {
						toEmit = append(toEmit, pending{name: flatName, schema: nested[1].(*Schema)})
					}
				}
				b.WriteString(fmt.Sprintf("    %s alt_%d = %d;\n", pt, idx, fieldNum))
				fieldNum++
			}
			b.WriteString("  }\n")
		}
	}

	b.WriteString("}\n\n")
	// Emit deferred nested schemas top-level after parent
	for _, p := range toEmit {
		g.emitSchema(b, p.name, p.schema)
	}
}

func mergeInto(base *Schema, add *Schema) *Schema {
	if base.Properties == nil {
		base.Properties = map[string]*Schema{}
	}
	for k, v := range add.Properties {
		base.Properties[k] = v
	}
	return base
}

func (g *genContext) fieldType(name string, s *Schema) (string, []any) {
	s = g.resolveRef(s)
	if len(s.Enum) > 0 {
		return normalizeMessage(name), []any{normalizeMessage(name), s}
	}
	switch s.Type {
	case "string":
		if s.Format == "byte" || s.Format == "binary" {
			return "bytes", nil
		}
		return "string", nil
	case "integer":
		if s.Format == "int32" {
			return "int32", nil
		}
		return "int64", nil
	case "number":
		if s.Format == "float" {
			return "float", nil
		}
		return "double", nil
	case "boolean":
		return "bool", nil
	case "array":
		if s.Items == nil {
			return "repeated string", nil
		}
		et, nested := g.fieldType(name+"_item", s.Items)
		if nested != nil {
			return "repeated " + nested[0].(string), nested
		}
		return "repeated " + et, nil
	case "object":
		if len(s.Properties) == 0 && s.AddlProps != nil { // map
			vt, nested := g.fieldType(name+"_value", s.AddlProps)
			if nested != nil {
				return fmt.Sprintf("map<string,%s>", nested[0].(string)), nested
			}
			return fmt.Sprintf("map<string,%s>", vt), nil
		}
		return normalizeMessage(name), []any{normalizeMessage(name), s}
	default:
		if s.OneOf != nil || s.AllOf != nil || s.AnyOf != nil {
			return normalizeMessage(name), []any{normalizeMessage(name), s}
		}
	}
	return "string", nil
}

func (g *genContext) scalarType(s *Schema) string {
	s = g.resolveRef(s)
	switch s.Type {
	case "string":
		return "string"
	case "integer":
		if s.Format == "int32" {
			return "int32"
		}
		return "int64"
	case "number":
		if s.Format == "float" {
			return "float"
		}
		return "double"
	case "boolean":
		return "bool"
	}
	return "string"
}

func (g *genContext) resolveRef(s *Schema) *Schema {
	if s == nil {
		return &Schema{}
	}
	if s.Ref == "" {
		return s
	}
	// expecting '#/components/schemas/Name'
	parts := strings.Split(s.Ref, "/")
	if len(parts) > 0 {
		key := parts[len(parts)-1]
		if tgt, ok := g.doc.Components.Schemas[key]; ok {
			return tgt
		}
	}
	return s
}

func normalizeMessage(name string) string {
	name = nonAlnumReplace(name)
	return upperCamel(name)
}
func normalizeField(name string) string {
	name = nonAlnumReplace(name)
	return lowerSnake(name)
}

func nonAlnumReplace(s string) string {
	r := strings.NewReplacer("-", "_", ".", "_", " ", "_")
	return r.Replace(s)
}

func toEnumValue(s string) string {
	s = strings.ToUpper(s)
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

func upperCamel(s string) string {
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		// Keep all-caps acronyms (length>1) as-is
		if isAllUpper(p) && len(p) > 1 {
			parts[i] = p
			continue
		}
		if len(p) == 1 {
			parts[i] = strings.ToUpper(p)
			continue
		}
		// Capitalize first rune, preserve existing inner capitalization (do not force lowercase)
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

func isAllUpper(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			return false
		}
	}
	return true
}
func lowerSnake(s string) string {
	s = strings.TrimSpace(s)
	var out []rune
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out = append(out, '_')
			}
			out = append(out, r-'A'+'a')
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out = append(out, r)
			continue
		}
		out = append(out, '_')
	}
	return strings.Trim(outStr(out), "_")
}
func outStr(r []rune) string { return string(r) }

func oneline(s string) string { s = strings.ReplaceAll(s, "\n", " "); return strings.TrimSpace(s) }

func isScalar(t string) bool {
	switch t {
	case "string", "int32", "int64", "double", "float", "bool", "bytes":
		return true
	}
	return false
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "error:", err); os.Exit(1) }

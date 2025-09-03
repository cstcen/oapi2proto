package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Simplified OAS structures (minimal fields used)
type Document struct {
	Components struct {
		Schemas   map[string]*Schema   `json:"schemas" yaml:"schemas"`
		Responses map[string]*Response `json:"responses" yaml:"responses"`
	} `json:"components" yaml:"components"`
}

type Schema struct {
	Ref         string                `json:"$ref" yaml:"$ref"`
	Type        string                `json:"type" yaml:"type"`
	Format      string                `json:"format" yaml:"format"`
	Enum        []string              `json:"enum" yaml:"enum"`
	Properties  map[string]*Schema    `json:"properties" yaml:"properties"`
	Items       *Schema               `json:"items" yaml:"items"`
	OneOf       []*Schema             `json:"oneOf" yaml:"oneOf"`
	AllOf       []*Schema             `json:"allOf" yaml:"allOf"`
	AnyOf       []*Schema             `json:"anyOf" yaml:"anyOf"`
	Required    []string              `json:"required" yaml:"required"`
	Nullable    bool                  `json:"nullable" yaml:"nullable"`
	AddlProps   *AdditionalProperties `json:"additionalProperties" yaml:"additionalProperties"`
	Description string                `json:"description" yaml:"description"`
}

// Response represents OpenAPI Response object under components.responses
type Response struct {
	Ref         string                        `json:"$ref" yaml:"$ref"`
	Description string                        `json:"description" yaml:"description"`
	Content     map[string]*ResponseMediaType `json:"content" yaml:"content"`
}

// ResponseMediaType represents a media type entry with an associated schema
type ResponseMediaType struct {
	Schema *Schema `json:"schema" yaml:"schema"`
}

// AdditionalProperties can be a schema object or a boolean (true/false)
type AdditionalProperties struct {
	Schema *Schema
	Bool   *bool
}

// UnmarshalYAML implements yaml unmarshaling for AdditionalProperties
func (a *AdditionalProperties) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var b bool
		if err := value.Decode(&b); err != nil {
			return err
		}
		a.Bool = &b
		return nil
	case yaml.MappingNode:
		var s Schema
		if err := value.Decode(&s); err != nil {
			return err
		}
		a.Schema = &s
		return nil
	default:
		return nil
	}
}

// UnmarshalJSON implements json unmarshaling for AdditionalProperties
func (a *AdditionalProperties) UnmarshalJSON(data []byte) error {
	// try boolean first
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		a.Bool = &b
		return nil
	}
	// then schema
	var s Schema
	if err := json.Unmarshal(data, &s); err == nil {
		a.Schema = &s
		return nil
	}
	return nil
}

func main() {
	in := flag.String("in", "openapi.json", "openapi v3 文件或目录 (json|yaml|yml)")
	out := flag.String("out", "api.proto", "输出 proto 文件 (单文件模式) 或目录 (目录输入模式)")
	pkg := flag.String("pkg", "api.v1", "proto package")
	goPkg := flag.String("go_pkg", "example.com/project/api/v1;v1", "go_package option value")
	useOptional := flag.Bool("use-optional", true, "为 nullable 标量生成 optional")
	anyOfMode := flag.String("anyof", "oneof", "anyof 处理: oneof|repeat")
	sortFields := flag.Bool("sort", true, "按字母排序 schema 与字段以获得稳定结果")
	preserveNames := flag.Bool("preserve-names", false, "保留 schema 名称作为 proto 类型名 (不进行驼峰化)")
	freeform := flag.String("freeform", "value", "additionalProperties: true 的值类型: string|value|any (默认 value=google.protobuf.Value)")
	parallel := flag.Int("parallel", 0, "并行文件数量 (0=auto,1=串行)")
	flag.Parse()

	info, err := os.Stat(*in)
	if err != nil {
		fatal(err)
	}

	// 单文件行为维持原样
	if !info.IsDir() {
		if err := generateForFile(*in, *out, *pkg, *goPkg, *useOptional, *anyOfMode, *sortFields, *preserveNames, *freeform); err != nil {
			fatal(err)
		}
		return
	}
	// 是否合并为单一 proto 文件: 目录输入 + 输出以 .proto 结尾
	combine := strings.HasSuffix(strings.ToLower(*out), ".proto")

	// 收集文件 (目录模式通用)
	var files []string
	walkFn := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		lower := strings.ToLower(d.Name())
		if strings.HasSuffix(lower, ".json") || strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") {
			files = append(files, p)
		}
		return nil
	}
	if err := filepath.WalkDir(*in, walkFn); err != nil {
		fatal(err)
	}
	if len(files) == 0 {
		fatal(errors.New("目录下未找到 openapi 文件 (*.json|*.yaml|*.yml)"))
	}
	sort.Strings(files)

	if combine {
		if err := generateCombined(files, *out, *pkg, *goPkg, *useOptional, *anyOfMode, *sortFields, *preserveNames, *freeform); err != nil {
			fatal(err)
		}
		return
	}

	// 分散模式: 输出目录
	outDir := *out
	if strings.HasSuffix(strings.ToLower(outDir), ".proto") { // 用户给了 proto 但我们非 combine 模式 (理论不会触发)
		outDir = filepath.Dir(outDir)
		if outDir == "." || outDir == "" {
			outDir = "protos"
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatal(err)
	}

	workerN := *parallel
	if workerN <= 0 {
		if len(files) <= 4 {
			workerN = len(files)
		} else {
			workerN = 4
		}
	}
	if workerN < 1 {
		workerN = 1
	}
	type job struct{ inFile string }
	type result struct {
		file string
		err  error
	}
	jobs := make(chan job)
	results := make(chan result)
	for w := 0; w < workerN; w++ {
		go func() {
			for j := range jobs {
				base := filepath.Base(j.inFile)
				base = strings.TrimSuffix(base, filepath.Ext(base))
				outFile := filepath.Join(outDir, base+".proto")
				rErr := generateForFile(j.inFile, outFile, *pkg, *goPkg, *useOptional, *anyOfMode, *sortFields, *preserveNames, *freeform)
				results <- result{file: j.inFile, err: rErr}
			}
		}()
	}
	go func() {
		for _, f := range files {
			jobs <- job{inFile: f}
		}
		close(jobs)
	}()
	var failed []string
	for i := 0; i < len(files); i++ {
		r := <-results
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "[FAIL] %s: %v\n", r.file, r.err)
			failed = append(failed, r.file)
		} else {
			fmt.Fprintf(os.Stderr, "[OK]   %s (%s)\n", r.file, time.Now().Format("15:04:05"))
		}
	}
	if len(failed) > 0 {
		fatal(fmt.Errorf("%d 个文件生成失败", len(failed)))
	}
}

// generateForFile 处理单个 openapi 文件 -> proto
func generateForFile(inFile, outFile, pkg, goPkg string, useOptional bool, anyOfMode string, sortFields bool, preserveNames bool, freeform string) error {
	data, err := os.ReadFile(inFile)
	if err != nil {
		return err
	}
	doc, err := parseDocument(data)
	if err != nil {
		return fmt.Errorf("%s: %w", inFile, err)
	}
	var b strings.Builder
	b.WriteString("syntax = \"proto3\";\n")
	b.WriteString(fmt.Sprintf("package %s;\n", pkg))
	b.WriteString(fmt.Sprintf("option go_package = \"%s\";\n", goPkg))

	names := make([]string, 0, len(doc.Components.Schemas))
	for name := range doc.Components.Schemas {
		names = append(names, name)
	}
	if sortFields {
		sort.Strings(names)
	}
	ctx := &genContext{doc: &doc, useOptional: useOptional, anyOfMode: anyOfMode, sortFields: sortFields, preserveNames: preserveNames, freeform: freeform, visited: map[string]bool{}, imports: map[string]bool{}}
	for _, name := range names {
		ctx.emitSchema(&b, name, doc.Components.Schemas[name])
	}
	// Emit component responses as messages too (prefer application/json schema)
	if doc.Components.Responses != nil && len(doc.Components.Responses) > 0 {
		rnames := make([]string, 0, len(doc.Components.Responses))
		for rn := range doc.Components.Responses {
			rnames = append(rnames, rn)
		}
		if sortFields {
			sort.Strings(rnames)
		}
		for _, rn := range rnames {
			r := resolveResponseRef(&doc, doc.Components.Responses[rn])
			if r == nil || r.Content == nil {
				continue
			}
			var sch *Schema
			if mt, ok := r.Content["application/json"]; ok && mt != nil {
				sch = mt.Schema
			} else {
				for _, mt := range r.Content {
					if mt != nil && mt.Schema != nil {
						sch = mt.Schema
						break
					}
				}
			}
			if sch == nil {
				continue
			}
			ctx.emitSchema(&b, rn, sch)
		}
	}
	// emit imports if needed
	if len(ctx.imports) > 0 {
		var imps []string
		for p := range ctx.imports {
			imps = append(imps, p)
		}
		sort.Strings(imps)
		var ib strings.Builder
		for _, p := range imps {
			ib.WriteString(fmt.Sprintf("import \"%s\";\n", p))
		}
		content := b.String()
		insertAt := -1
		if idx := strings.Index(content, "option go_package"); idx >= 0 {
			if end := strings.Index(content[idx:], "\n"); end >= 0 {
				insertAt = idx + end + 1
			}
		}
		if insertAt == -1 {
			// after package line
			// find first two newlines (syntax line and package line)
			first := strings.Index(content, "\n")
			if first >= 0 {
				secondRel := strings.Index(content[first+1:], "\n")
				if secondRel >= 0 {
					insertAt = first + 1 + secondRel + 1
				}
			}
		}
		if insertAt == -1 {
			insertAt = len(content)
		}
		output := content[:insertAt] + ib.String() + content[insertAt:]
		b.Reset()
		b.WriteString(output)
	} else {
		b.WriteString("\n")
	}
	if err := os.MkdirAll(filepath.Dir(outFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(outFile, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return nil
}

// parseDocument 尝试 json / yaml
func parseDocument(data []byte) (Document, error) {
	var doc Document
	var jsonErr error
	if jErr := json.Unmarshal(data, &doc); jErr != nil || len(doc.Components.Schemas) == 0 {
		jsonErr = jErr
		var ydoc Document
		yErr := yaml.Unmarshal(data, &ydoc)
		if yErr == nil { // accept YAML even if schemas are empty (responses-only)
			doc = ydoc
		} else if jsonErr != nil {
			return Document{}, fmt.Errorf("parse openapi (json/yaml) failed: jsonErr=%v yamlErr=%v", jsonErr, yErr)
		}
	}
	// Ensure non-nil maps
	if doc.Components.Schemas == nil {
		doc.Components.Schemas = map[string]*Schema{}
	}
	if doc.Components.Responses == nil {
		doc.Components.Responses = map[string]*Response{}
	}
	if len(doc.Components.Schemas) == 0 && len(doc.Components.Responses) == 0 {
		return Document{}, errors.New("no components.schemas or components.responses found")
	}
	return doc, nil
}

// generateCombined 聚合多个 openapi 文件为单一 proto，重复 schema 名只保留首次出现
func generateCombined(files []string, outFile, pkg, goPkg string, useOptional bool, anyOfMode string, sortFields bool, preserveNames bool, freeform string) error {
	combined := Document{}
	combined.Components.Schemas = map[string]*Schema{}
	combined.Components.Responses = map[string]*Response{}
	overridden := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		doc, err := parseDocument(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[WARN] 跳过 %s: %v\n", f, err)
			continue
		}
		for name, schema := range doc.Components.Schemas {
			if _, exists := combined.Components.Schemas[name]; exists {
				overridden++
			}
			combined.Components.Schemas[name] = schema // 后者覆盖前者
		}
		for name, resp := range doc.Components.Responses {
			if _, exists := combined.Components.Responses[name]; exists {
				overridden++
			}
			combined.Components.Responses[name] = resp
		}
	}
	if len(combined.Components.Schemas) == 0 && len(combined.Components.Responses) == 0 {
		return errors.New("无有效 schema/response 可生成")
	}
	var b strings.Builder
	b.WriteString("syntax = \"proto3\";\n")
	b.WriteString(fmt.Sprintf("package %s;\n", pkg))
	b.WriteString(fmt.Sprintf("option go_package = \"%s\";\n", goPkg))
	if overridden > 0 {
		b.WriteString(fmt.Sprintf("// 注意: 有 %d 个重复 schema 名被后续文件覆盖 (采用最后出现版本)\n\n", overridden))
	}
	names := make([]string, 0, len(combined.Components.Schemas))
	for n := range combined.Components.Schemas {
		names = append(names, n)
	}
	if sortFields {
		sort.Strings(names)
	}
	ctx := &genContext{doc: &combined, useOptional: useOptional, anyOfMode: anyOfMode, sortFields: sortFields, preserveNames: preserveNames, freeform: freeform, visited: map[string]bool{}, imports: map[string]bool{}}
	for _, n := range names {
		ctx.emitSchema(&b, n, combined.Components.Schemas[n])
	}
	// Emit responses
	if len(combined.Components.Responses) > 0 {
		rnames := make([]string, 0, len(combined.Components.Responses))
		for n := range combined.Components.Responses {
			rnames = append(rnames, n)
		}
		if sortFields {
			sort.Strings(rnames)
		}
		for _, rn := range rnames {
			r := resolveResponseRef(&combined, combined.Components.Responses[rn])
			if r == nil || r.Content == nil {
				continue
			}
			var sch *Schema
			if mt, ok := r.Content["application/json"]; ok && mt != nil {
				sch = mt.Schema
			} else {
				for _, mt := range r.Content {
					if mt != nil && mt.Schema != nil {
						sch = mt.Schema
						break
					}
				}
			}
			if sch == nil {
				continue
			}
			ctx.emitSchema(&b, rn, sch)
		}
	}
	// emit imports if needed
	if len(ctx.imports) > 0 {
		var imps []string
		for p := range ctx.imports {
			imps = append(imps, p)
		}
		sort.Strings(imps)
		var ib strings.Builder
		for _, p := range imps {
			ib.WriteString(fmt.Sprintf("import \"%s\";\n", p))
		}
		content := b.String()
		insertAt := -1
		if idx := strings.Index(content, "option go_package"); idx >= 0 {
			if end := strings.Index(content[idx:], "\n"); end >= 0 {
				insertAt = idx + end + 1
			}
		}
		if insertAt == -1 {
			first := strings.Index(content, "\n")
			if first >= 0 {
				secondRel := strings.Index(content[first+1:], "\n")
				if secondRel >= 0 {
					insertAt = first + 1 + secondRel + 1
				}
			}
		}
		if insertAt == -1 {
			insertAt = len(content)
		}
		output := content[:insertAt] + ib.String() + content[insertAt:]
		b.Reset()
		b.WriteString(output)
	} else {
		b.WriteString("\n")
	}
	if err := os.MkdirAll(filepath.Dir(outFile), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outFile, []byte(b.String()), 0o644)
}

type genContext struct {
	doc           *Document
	useOptional   bool
	anyOfMode     string
	sortFields    bool
	preserveNames bool
	freeform      string
	visited       map[string]bool
	imports       map[string]bool
}

func (g *genContext) emitSchema(b *strings.Builder, name string, s *Schema) {
	if g.visited[name] {
		return
	}
	g.visited[name] = true
	resolved := g.resolveRef(s)
	if len(resolved.Enum) > 0 {
		// Do not generate enums at all; treat as underlying scalar type
		// fall through to primitive wrapper generation or object handling
	}
	if resolved.Type == "object" || resolved.Properties != nil || resolved.AllOf != nil || resolved.OneOf != nil || resolved.AnyOf != nil || resolved.AddlProps != nil {
		g.emitMessage(b, name, resolved)
		return
	}
	// Primitive at top-level: wrap in message
	b.WriteString(fmt.Sprintf("// Primitive schema %s promoted to wrapper message\n", name))
	b.WriteString(fmt.Sprintf("message %s { %s value = 1; }\n\n", g.msgName(name), g.scalarType(resolved)))
}

func (g *genContext) emitEnum(b *strings.Builder, name string, s *Schema) {
	enumName := g.msgName(name)
	b.WriteString(fmt.Sprintf("enum %s {\n", enumName))
	b.WriteString(fmt.Sprintf("  %s_UNSPECIFIED = 0;\n", strings.ToUpper(enumName)))
	for i, v := range s.Enum {
		b.WriteString(fmt.Sprintf("  %s_%s = %d;\n", strings.ToUpper(enumName), toEnumValue(v), i+1))
	}
	b.WriteString("}\n\n")
}

func (g *genContext) emitMessage(b *strings.Builder, name string, s *Schema) {
	msgName := g.msgName(name)
	b.WriteString(fmt.Sprintf("message %s {\n", msgName))
	// Expand/flatten allOf semantics (inheritance/aggregation)
	merged := g.flattenAllOf(s)

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
			flatName := g.msgName(msgName + "_" + baseNestedName)
			// Preserve qualifiers like "repeated" or "map<...>" by replacing only the nested type token
			ptype = strings.ReplaceAll(ptype, baseNestedName, flatName)
			// schedule emission if not visited yet under new name
			if !g.visited[flatName] {
				toEmit = append(toEmit, pending{name: flatName, schema: nested[1].(*Schema)})
			}
		}
		opt := ""
		if ps.Nullable && g.useOptional && isScalar(ptype) {
			opt = "optional "
		}
		b.WriteString(fmt.Sprintf("  %s%s %s = %d;", opt, ptype, sanitizeFieldName(prop), fieldNum))
		if ps.Description != "" {
			b.WriteString(fmt.Sprintf(" // %s", oneline(ps.Description)))
		}
		b.WriteString("\n")
		fieldNum++
	}

	// map type
	if merged.AddlProps != nil && len(merged.Properties) == 0 {
		if merged.AddlProps.Schema != nil {
			valType, nested := g.fieldType("value", merged.AddlProps.Schema)
			if nested != nil {
				g.emitSchema(b, nested[0].(string), nested[1].(*Schema))
			}
			b.WriteString(fmt.Sprintf("  map<string,%s> entries = 1;\n", valType))
		} else if merged.AddlProps.Bool != nil && *merged.AddlProps.Bool {
			// Free-form object -> map value depends on freeform mode
			switch g.freeform {
			case "any":
				g.imports["google/protobuf/any.proto"] = true
				b.WriteString("  map<string,google.protobuf.Any> entries = 1;\n")
			case "string":
				b.WriteString("  map<string,string> entries = 1;\n")
			default: // value
				g.imports["google/protobuf/struct.proto"] = true
				b.WriteString("  map<string,google.protobuf.Value> entries = 1;\n")
			}
		}
	}

	// oneOf -> oneof block
	if len(merged.OneOf) > 0 {
		b.WriteString("  oneof one_of {\n")
		idx := 0
		for _, branch := range merged.OneOf {
			idx++
			pt, nested := g.fieldType(fmt.Sprintf("choice_%d", idx), branch)
			if nested != nil {
				baseNestedName := nested[0].(string)
				flatName := g.msgName(msgName + "_" + baseNestedName)
				pt = strings.ReplaceAll(pt, baseNestedName, flatName)
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
	if len(merged.AnyOf) > 0 {
		if g.anyOfMode == "repeat" {
			pt, nested := g.fieldType("anyof_value", merged.AnyOf[0])
			if nested != nil {
				baseNestedName := nested[0].(string)
				flatName := g.msgName(msgName + "_" + baseNestedName)
				pt = strings.ReplaceAll(pt, baseNestedName, flatName)
				if !g.visited[flatName] {
					toEmit = append(toEmit, pending{name: flatName, schema: nested[1].(*Schema)})
				}
			}
			b.WriteString(fmt.Sprintf("  repeated %s anyof_value = %d; // anyOf first schema repeated\n", pt, fieldNum))
			fieldNum++
		} else {
			b.WriteString("  oneof any_of {\n")
			idx := 0
			for _, branch := range merged.AnyOf {
				idx++
				pt, nested := g.fieldType(fmt.Sprintf("alt_%d", idx), branch)
				if nested != nil {
					baseNestedName := nested[0].(string)
					flatName := g.msgName(msgName + "_" + baseNestedName)
					pt = strings.ReplaceAll(pt, baseNestedName, flatName)
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

// flattenAllOf expands OpenAPI allOf by recursively merging referenced/object schemas.
// Precedence: later parts override earlier ones on key conflicts.
func (g *genContext) flattenAllOf(s *Schema) *Schema {
	if s == nil {
		return &Schema{Properties: map[string]*Schema{}}
	}
	s = g.resolveRef(s)
	// Start with a shallow copy to preserve direct fields
	out := &Schema{
		Type:        s.Type,
		Format:      s.Format,
		Enum:        append([]string(nil), s.Enum...),
		Properties:  map[string]*Schema{},
		Items:       s.Items,
		OneOf:       s.OneOf,
		AllOf:       nil, // will be flattened
		AnyOf:       s.AnyOf,
		Required:    append([]string(nil), s.Required...),
		Nullable:    s.Nullable,
		AddlProps:   s.AddlProps,
		Description: s.Description,
	}
	// Merge parts from allOf first (inheritance base first, then extensions)
	for _, part := range s.AllOf {
		g.mergeSchema(out, g.flattenAllOf(part))
	}
	// Then merge direct properties/fields from s itself (extension)
	for k, v := range s.Properties {
		out.Properties[k] = v
	}
	// Ensure object type when properties exist
	if out.Type == "" && (len(out.Properties) > 0 || out.AddlProps != nil || len(out.OneOf) > 0 || len(out.AnyOf) > 0) {
		out.Type = "object"
	}
	// Dedup required
	if len(out.Required) > 1 {
		out.Required = uniqStrings(out.Required)
	}
	return out
}

// mergeSchema merges src into dst. Later values override earlier ones for property keys.
func (g *genContext) mergeSchema(dst *Schema, src *Schema) {
	if dst == nil || src == nil {
		return
	}
	src = g.resolveRef(src)
	if dst.Properties == nil {
		dst.Properties = map[string]*Schema{}
	}
	for k, v := range src.Properties {
		dst.Properties[k] = v
	}
	// Union required
	if len(src.Required) > 0 {
		dst.Required = append(dst.Required, src.Required...)
	}
	// Prefer non-nil additionalProperties
	if src.AddlProps != nil {
		dst.AddlProps = src.AddlProps
	}
	// If type not set on dst, adopt src type (useful when allOf only provides type)
	if dst.Type == "" && src.Type != "" {
		dst.Type = src.Type
	}
}

func uniqStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func (g *genContext) fieldType(name string, s *Schema) (string, []any) {
	rawRef := ""
	if s != nil {
		rawRef = s.Ref
	}
	s = g.resolveRef(s)
	// Any enums are treated as their underlying scalar proto type
	if len(s.Enum) > 0 {
		return g.scalarType(s), nil
	}
	// Derive effective type: some schemas omit 'type: object' but provide properties
	effType := s.Type
	if effType == "" {
		if len(s.Properties) > 0 || s.AddlProps != nil || s.OneOf != nil || s.AllOf != nil || s.AnyOf != nil {
			effType = "object"
		} else if s.Items != nil {
			effType = "array"
		}
	}
	switch effType {
	case "string":
		if s.Format == "byte" || s.Format == "binary" {
			return "bytes", nil
		}
		return "string", nil
	case "int":
		// Non-standard but seen in the wild; default to 64-bit
		return "int64", nil
	case "long":
		return "int64", nil
	case "integer":
		if s.Format == "int32" {
			return "int32", nil
		}
		return "int64", nil
	case "float":
		return "float", nil
	case "double":
		return "double", nil
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
			if s.AddlProps.Schema != nil {
				vt, nested := g.fieldType(name+"_value", s.AddlProps.Schema)
				if nested != nil {
					return fmt.Sprintf("map<string,%s>", nested[0].(string)), nested
				}
				return fmt.Sprintf("map<string,%s>", vt), nil
			}
			if s.AddlProps.Bool != nil && *s.AddlProps.Bool {
				switch g.freeform {
				case "any":
					g.imports["google/protobuf/any.proto"] = true
					return "map<string,google.protobuf.Any>", nil
				case "string":
					return "map<string,string>", nil
				default:
					g.imports["google/protobuf/struct.proto"] = true
					return "map<string,google.protobuf.Value>", nil
				}
			}
		}
		// If this object schema composes via allOf, always embed as a nested type
		if len(s.AllOf) > 0 {
			return g.msgName(name), []any{g.msgName(name), s}
		}
		// Prefer referenced name if this object came from a $ref
		if rawRef != "" {
			if key := refKeyToName(rawRef); key != "" {
				if _, ok := g.doc.Components.Schemas[key]; ok {
					return g.msgName(key), nil
				}
			}
		}
		return g.msgName(name), []any{g.msgName(name), s}
	default:
		if s.OneOf != nil || s.AllOf != nil || s.AnyOf != nil {
			// If this is a composition by $ref, use the referenced name
			if rawRef != "" {
				if key := refKeyToName(rawRef); key != "" {
					if _, ok := g.doc.Components.Schemas[key]; ok {
						return g.msgName(key), nil
					}
				}
			}
			return g.msgName(name), []any{g.msgName(name), s}
		}
	}
	return "string", nil
}

// refKeyToName extracts the schema key from a $ref like '#/components/schemas/Name' or '#Name'.
func refKeyToName(ref string) string {
	if ref == "" {
		return ""
	}
	if i := strings.Index(ref, "#"); i >= 0 {
		ref = ref[i+1:]
	}
	ref = strings.TrimPrefix(ref, "/")
	if ref == "" {
		return ""
	}
	parts := strings.Split(ref, "/")
	key := parts[len(parts)-1]
	key = strings.ReplaceAll(key, "~1", "/")
	key = strings.ReplaceAll(key, "~0", "~")
	return key
}

func (g *genContext) scalarType(s *Schema) string {
	s = g.resolveRef(s)
	switch s.Type {
	case "string":
		return "string"
	case "int":
		return "int64"
	case "long":
		return "int64"
	case "integer":
		if s.Format == "int32" {
			return "int32"
		}
		return "int64"
	case "float":
		return "float"
	case "double":
		return "double"
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
	// Support fragments like '#/components/schemas/Name', '#/definitions/Name', and '#Name'
	ref := s.Ref
	// Strip URL part before '#'
	if i := strings.Index(ref, "#"); i >= 0 {
		ref = ref[i+1:]
	}
	ref = strings.TrimPrefix(ref, "/")
	if ref != "" {
		parts := strings.Split(ref, "/")
		key := parts[len(parts)-1]
		if key != "" {
			// Unescape JSON Pointer tokens
			key = strings.ReplaceAll(key, "~1", "/")
			key = strings.ReplaceAll(key, "~0", "~")
			if tgt, ok := g.doc.Components.Schemas[key]; ok {
				return tgt
			}
		}
		// Fallback: if there are no '/', treat entire ref as key (e.g., '#Name')
		if len(parts) == 1 {
			if tgt, ok := g.doc.Components.Schemas[ref]; ok {
				return tgt
			}
		}
	}
	return s
}

// resolveResponseRef resolves a components.responses $ref like '#/components/responses/Name' or '#Name'
func resolveResponseRef(doc *Document, r *Response) *Response {
	if r == nil || r.Ref == "" {
		return r
	}
	ref := r.Ref
	if i := strings.Index(ref, "#"); i >= 0 {
		ref = ref[i+1:]
	}
	ref = strings.TrimPrefix(ref, "/")
	if ref == "" {
		return r
	}
	parts := strings.Split(ref, "/")
	key := parts[len(parts)-1]
	key = strings.ReplaceAll(key, "~1", "/")
	key = strings.ReplaceAll(key, "~0", "~")
	if tgt, ok := doc.Components.Responses[key]; ok {
		return tgt
	}
	// fallback: single-part key
	if len(parts) == 1 {
		if tgt, ok := doc.Components.Responses[ref]; ok {
			return tgt
		}
	}
	return r
}

// normalizeMessage removes all non-letter characters and applies UpperCamel casing
func normalizeMessage(name string) string {
	var out []rune
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			out = append(out, r)
		}
	}
	return upperCamel(string(out))
}
func normalizeField(name string) string {
	name = nonAlnumReplace(name)
	return lowerSnake(name)
}

// msgName returns the proto message/enum type name. If preserveNames is true,
// it keeps the original schema name (only replacing illegal chars with underscores),
// otherwise it applies UpperCamel casing like normalizeMessage.
func (g *genContext) msgName(name string) string {
	if g.preserveNames {
		// Remove all non-letter characters for schema names
		var out []rune
		for _, r := range name {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return "Message"
		}
		return string(out)
	}
	return normalizeMessage(name)
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

// sanitizeFieldName preserves the original property name while ensuring it's a valid proto identifier.
// - If it starts with a digit, prefix with "f_".
// - Replace illegal characters with '_'.
// - Avoid empty names by falling back to "field".
func sanitizeFieldName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "field"
	}
	// Replace invalid chars
	var out []rune
	for i, r := range s {
		valid := (r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
		if i == 0 {
			// First char cannot be a digit
			if r >= '0' && r <= '9' {
				out = append(out, 'f', '_', r)
				continue
			}
		}
		if valid {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	// Ensure first char is not digit (in case original started with '_'+digit only)
	if len(out) > 0 && out[0] >= '0' && out[0] <= '9' {
		out = append([]rune{'f', '_'}, out...)
	}
	res := string(out)
	if strings.Trim(res, "_") == "" {
		return "field"
	}
	return res
}

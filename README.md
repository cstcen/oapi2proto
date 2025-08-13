# OpenAPI v3 -> Proto3 Minimal Generator

Purpose: Convert a subset of OpenAPI 3.0 schemas under `components.schemas` into one or more starting `.proto` files for further manual refinement. Supports JSON or YAML input transparently.

Status: MVP (objects, enums, arrays, maps, oneOf→oneof, anyOf (configurable), allOf (merge), primitive formats, `$ref` resolution, directory batch + combined merge generation).

## Quick Start

```bash
# build binary
go build -o oapi2proto ./cmd/oapi2proto

# single file -> single proto
./oapi2proto -in openapi.yaml -out api.proto -pkg api.v1 -go_pkg example.com/project/api/v1;v1

# directory -> many proto files (one per OpenAPI file)
./oapi2proto -in ./specs -out ./protos -pkg api.v1 -go_pkg example.com/project/api/v1;v1 -parallel 4

# directory -> single merged proto (duplicate schema names: later files override earlier ones)
./oapi2proto -in ./specs -out merged.proto -pkg api.v1 -go_pkg example.com/project/api/v1;v1
```

## Flags

| Flag | Description |
|------|-------------|
| `-in` | OpenAPI file or directory containing multiple OpenAPI files (.json/.yaml/.yml). |
| `-out` | Output proto file (single-file input) OR output directory (multi-file mode). If `-in` is a directory and `-out` ends with `.proto`, a single merged proto is produced. |
| `-pkg` | Proto `package` name. |
| `-go_pkg` | Value for `option go_package`. |
| `-use-optional` | Emit `optional` for nullable scalar fields (default true). |
| `-anyof` | `oneof` (default) or `repeat` (repeat first schema). |
| `-sort` | Alphabetically sort schemas & fields for stable diffs (default true). |
| `-parallel` | Worker count for per-file generation (directory multi-file mode). `0` = auto. Ignored in merged mode. |

## Modes

1. Single File Mode: `-in` points to a JSON/YAML file → one proto file.
2. Directory Multi-File Mode: `-in` directory & `-out` is directory → each OpenAPI file generates a separate proto with same basename.
3. Directory Merge Mode: `-in` directory & `-out` ends with `.proto` → all schemas merged into a single file. Duplicate schema names: later files override earlier (annotated in header comment with override count).

## Behavior Details

| Feature | Behavior |
|---------|----------|
| `$ref` | Only resolves local refs of form `#/components/schemas/Name`. |
| `allOf` | Merges object properties shallowly (later overwrites keys). |
| `oneOf` | Proto `oneof one_of { ... }`. |
| `anyOf` | Treated like `oneof` or `repeated <first-type>` depending on `-anyof`. |
| `enum` | Creates `ENUM_NAME_UNSPECIFIED = 0` + uppercased variants. |
| `nullable` | Adds `optional` keyword for scalars if `-use-optional`. |
| Arrays | `repeated <T>`; nested object/enum becomes separate top-level message/enum with parent-based name prefix. |
| Maps | `type: object` with only `additionalProperties`. |
| Duplicate schema names (merge mode) | Later file overrides earlier definition. Count emitted as comment. |

## Scope & Limitations

- Only processes `components.schemas`; no service / RPC generation from `paths` yet.
- No remote `$ref` fetching (URLs / external files) currently.
- Inline nested objects produce flattened top-level messages with parent-name prefix (no reuse dedup among identical anonymous shapes yet).
- No structural conflict detection when overriding duplicates (last wins blindly).
- Field number allocation resets per run; renumbering changes are possible if schema set changes (even though sorting helps stability).

## Roadmap Ideas

- Generate services from `paths` with REST -> gRPC annotations (google.api.http) optionally.
- Add strategy flag for duplicate handling: first|last|error|hash-rename.
- Optional hash-based suffix to avoid message name collisions.
- Wrapper well-known types for nullable semantics.
- Deterministic field number registry file.
- Detect and reuse identical inline schemas.

## License

MVP example tool – adapt freely within project constraints.

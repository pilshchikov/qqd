# YAML Subset Supported by qqd

`qqd` ships with a small, dependency-free YAML parser. It is **not a full YAML 1.2 implementation**. This page documents exactly what is and isn't supported, so that you can either keep your config inside the supported subset or convert it to JSON or HOCON (also supported by `qqd`).

If you need full YAML, write your config in JSON instead - JSON is a strict subset of YAML and the JSON parser is the standard library `encoding/json`, which is fully spec-compliant.

## TL;DR

If your config looks like a Docker Compose file or a Kubernetes manifest's `data:` block, you're almost certainly fine. The features that real-world `qqd` users tend to want - nested maps, lists of strings, env values, quoted strings with `:` in them - all work.

The features that are explicitly **not** supported are the ones most often associated with "advanced YAML": anchors (`&foo`), aliases (`*foo`), merge keys (`<<:`), multi-line scalars (`|`, `>`), and tags (`!!str`). If you try to use them, the parser fails with a clear error rather than silently misinterpreting your config.

## Supported

### Maps

```yaml
services:
  server:
    image: server:1.0
  db:
    image: postgres:16
```

Indentation must be **two spaces per level**. Tabs are not supported.

### Sequences (lists)

Block form (one item per line):

```yaml
volumes:
  - /data:/data
  - /logs:/logs
```

Inline flow form (single line):

```yaml
command: ["sh", "-c", "echo hello"]
ports: [80, 443, 8080]
```

### Strings

Unquoted, single-quoted, and double-quoted forms all work:

```yaml
unquoted: hello world
single:   'with: a colon'
double:   "with: a colon"
```

A string containing `:`, `{`, `}`, `[`, `]`, `#`, `&`, `*`, `!`, `|`, `>`, `'`, `"`, `,`, `@`, or backtick must be quoted.

### Numbers

```yaml
replicas: 3
ratio:    1.5
```

The parser distinguishes integers from floats. JSON-style numbers also work.

### Booleans and null

```yaml
enabled: true
disabled: false
also_true: yes
also_false: no
nothing: null
also_nothing: ~
```

### Comments

Both whole-line and inline comments are supported:

```yaml
# whole-line comment
name: myproject  # inline comment
```

A `#` inside a quoted string is treated as data, not as a comment.

### Quoted keys

```yaml
"my.dotted.key": value
'also-quoted': value
```

### Variable references in values

`qqd` expands `${VAR}` and `${VAR:-default}` in values. This is done **after** YAML parsing, so the YAML parser itself doesn't need to know about it.

```yaml
env:
  DB_HOST: "${POSTGRES_HOST:-localhost}"
```

## Not supported

These are detected and rejected with a clear error rather than silently misinterpreted.

### Anchors and aliases

```yaml
defaults: &defaults    # NOT supported - rejected with: "YAML anchors (&name) are not supported"
  image: nginx:1
server: *defaults      # NOT supported - rejected with: "YAML aliases (*name) are not supported"
```

**Workaround:** inline the value, or use config overlays. `qqd` accepts repeated `-c` flags and deep-merges later overlays on top of earlier ones, which solves the same problem anchors solve in pure YAML.

### Multi-line scalars

```yaml
script: |        # NOT supported
  echo a
  echo b
```

**Workaround:** use a single-line quoted string with `\n` if you really need a newline, or move the script into a file and reference it (`file::path/to/script.sh`).

### YAML tags

```yaml
value: !!str 1   # NOT supported
custom: !MyType  # NOT supported
```

**Workaround:** quote the value to force a string, or rely on the inferred type.

### Merge keys

```yaml
service:
  <<: *base       # NOT supported
  image: x
```

**Workaround:** repeat the keys inline, or use a config overlay.

### Flow maps

```yaml
config: {a: 1, b: 2}   # NOT supported - rejected with: "YAML flow maps ({a: 1, b: 2}) are not supported"
```

**Workaround:** expand to a block map.

```yaml
config:
  a: 1
  b: 2
```

Note: flow **sequences** (`[1, 2, 3]`) are supported. Only flow maps are not.

### Multiple documents

```yaml
---       # NOT supported
name: a
---
name: b
```

**Workaround:** put each document in its own file and load them as overlays.

## Round-trip behavior

`qqd convert` round-trips a config through `parse -> emit` for the requested format. The YAML emitter is type-aware:

- A string `"1"` is emitted as `"1"` (quoted), preserving its string type on the next parse.
- An integer `1` is emitted as `1` (unquoted).
- A boolean `true` is emitted as `true`.
- Reserved-word strings (`"true"`, `"yes"`, `"null"`, etc.) are quoted to prevent reinterpretation.

Round-trips are covered by tests in `internal/qqd/yaml_subset_test.go`. If you find a config that parses correctly but emits as something different, please file a bug.

## Use JSON if you need stricter behavior

JSON is supported as a first-class config format. The JSON parser is `encoding/json` from the Go standard library and supports the full JSON spec.

```bash
qqd convert -c app.yaml -o app.json
qqd deploy  -c app.json
```

If you have an editor with YAML support and you want validation guarantees that go beyond what `qqd` provides, you can also keep your source as JSON Schema'd YAML and convert at deploy time.

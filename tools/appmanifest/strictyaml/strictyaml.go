// Package strictyaml reads dex-app.yaml by the one rule that every reader of
// the manifest shares: the platform imports this package, and every
// application carries the same source in tools/appmanifest.
//
// The file is one YAML document whose root is a mapping. Mappings, sequences,
// strings in any style, unsigned decimal integers, true and false, and null
// are accepted, so JSON is accepted as the YAML it is. Everything a YAML 1.1
// and a YAML 1.2 library could read differently is refused with its line and
// column: a second document, anchors, aliases, tags, merge keys, a repeated
// key, a key that is not a string, a date, a floating-point or signed number,
// and the plain values yes, no, on, off, y and n. Nesting deeper than 64,
// more than 100,000 values, a value over 64 KiB and an integer over
// 2147483647 are refused too.
package strictyaml

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"

	"gopkg.in/yaml.v3"
)

// MaxBytes bounds every dex-app.yaml a reader accepts. yaml.v3 refuses
// nesting deeper than 10,000 while it parses; the walk below bounds what it
// converts far lower.
const MaxBytes = 1 << 20

// The bounds of the converted document: its nesting, its nodes (each key,
// value and item), each scalar's bytes and each integer's value.
const (
	maxDepth       = 64
	maxNodes       = 100_000
	maxScalarBytes = 64 << 10
	maxInteger     = 2147483647
)

// CanonicalJSON returns the document as canonical JSON: UTF-8, object keys in
// sorted order, no insignificant whitespace, integers in decimal and strings
// with JSON's minimal escapes.
func CanonicalJSON(contents []byte) ([]byte, error) {
	if len(contents) > MaxBytes {
		return nil, fmt.Errorf("the manifest has %d bytes, more than the 1 MiB limit", len(contents))
	}
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the manifest is empty; it must be a YAML mapping")
		}
		return nil, fmt.Errorf("not valid YAML: %w", err)
	}
	var next yaml.Node
	if err := decoder.Decode(&next); !errors.Is(err, io.EOF) {
		return nil, errors.New("the manifest must hold exactly one YAML document; remove the second one")
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: the manifest must be a YAML mapping", position(root))
	}
	value, err := (&walk{}).value(root, 1)
	if err != nil {
		return nil, err
	}
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	if err = encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(canonical.Bytes(), []byte("\n")), nil
}

// The plain values the rule reads, and those it refuses because YAML 1.1 or
// YAML 1.2 reads them as something other than a string.
var (
	plainNulls     = map[string]bool{"": true, "~": true, "null": true, "Null": true, "NULL": true}
	plainBooleans  = map[string]bool{"true": true, "True": true, "TRUE": true, "false": false, "False": false, "FALSE": false}
	plainInteger   = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	plainFloat     = regexp.MustCompile(`^[-+]?([0-9][0-9_]*)?\.[0-9_]*([eE][-+]?[0-9]+)?$|^[-+]?[0-9][0-9_]*[eE][-+]?[0-9]+$|^[-+]?\.(?i:inf|nan)$`)
	plainDate      = regexp.MustCompile(`^[0-9]{4}-[0-9]{1,2}-[0-9]{1,2}([Tt ]|$)`)
	plainAmbiguous = regexp.MustCompile(`^(?i:y|yes|n|no|on|off)$|^(<<|=)$|^[-+]?\.?[0-9][0-9_.:eE+-]*$|^[-+]?0[xXoObB]`)
)

// walk converts one document's nodes, counting them against maxNodes.
type walk struct {
	nodes int
}

// value converts one node at depth (the root is 1) and its children to the
// value JSON encodes.
func (w *walk) value(node *yaml.Node, depth int) (any, error) {
	if w.nodes++; w.nodes > maxNodes {
		return nil, fmt.Errorf("%s: the manifest has more than %d values; a manifest declares far fewer", position(node), maxNodes)
	}
	if depth > maxDepth {
		return nil, fmt.Errorf("%s: the manifest nests deeper than %d levels", position(node), maxDepth)
	}
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return nil, fmt.Errorf("%s: YAML anchors and aliases are not allowed; write the value out", position(node))
	}
	if node.Style&yaml.TaggedStyle != 0 {
		return nil, fmt.Errorf("%s: the YAML tag %q is not allowed; remove it and quote a value that must be a string", position(node), node.Tag)
	}
	switch node.Kind {
	case yaml.MappingNode:
		return w.object(node, depth)
	case yaml.SequenceNode:
		items := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			item, err := w.value(child, depth+1)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		return items, nil
	case yaml.ScalarNode:
		return scalar(node)
	}
	return nil, fmt.Errorf("%s: unsupported YAML node", position(node))
}

// object converts a mapping whose keys are distinct strings.
func (w *walk) object(node *yaml.Node, depth int) (map[string]any, error) {
	object := make(map[string]any, len(node.Content)/2)
	keys := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		if key.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("%s: every mapping key must be a string; a complex key is not allowed", position(key))
		}
		name, err := w.value(key, depth+1)
		if err != nil {
			return nil, err
		}
		text, isString := name.(string)
		if !isString {
			return nil, fmt.Errorf("%s: every mapping key must be a string; quote %q", position(key), key.Value)
		}
		if first, repeated := keys[text]; repeated {
			return nil, fmt.Errorf("%s: the key %q repeats the key at %s; remove one", position(key), text, position(first))
		}
		keys[text] = key
		if object[text], err = w.value(value, depth+1); err != nil {
			return nil, err
		}
	}
	return object, nil
}

// scalar reads a quoted or block scalar as a string and a plain one by the
// rule.
func scalar(node *yaml.Node) (any, error) {
	if len(node.Value) > maxScalarBytes {
		return nil, fmt.Errorf("%s: the value has %d bytes, more than %d", position(node), len(node.Value), maxScalarBytes)
	}
	if node.Style != 0 {
		return node.Value, nil
	}
	text := node.Value
	switch value, isBoolean := plainBooleans[text]; {
	case plainNulls[text]:
		return nil, nil
	case isBoolean:
		return value, nil
	case plainInteger.MatchString(text):
		if integer, err := strconv.ParseInt(text, 10, 64); err != nil || integer > maxInteger {
			return nil, fmt.Errorf("%s: the integer %q is larger than %d", position(node), text, maxInteger)
		}
		return json.Number(text), nil
	case plainDate.MatchString(text):
		return nil, fmt.Errorf("%s: the plain value %q reads as a date; quote it", position(node), text)
	case plainFloat.MatchString(text):
		return nil, fmt.Errorf("%s: the floating-point number %q is not allowed, since no manifest field is fractional; quote it if it is a string", position(node), text)
	case plainAmbiguous.MatchString(text):
		return nil, fmt.Errorf("%s: YAML 1.1 and YAML 1.2 read the plain value %q differently; quote it", position(node), text)
	}
	return text, nil
}

// position names where node starts.
func position(node *yaml.Node) string {
	return fmt.Sprintf("line %d, column %d", node.Line, node.Column)
}

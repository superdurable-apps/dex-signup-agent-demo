package signup

// Tool argument checks: the zod schemas of src/signup-agent.ts, with zod's messages, on top of the JSON Schema the
// model sees. String lengths count UTF-16 code units, as JavaScript does.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// argumentField checks one member of an argument object.
type argumentField struct {
	name     string
	optional bool
	check    func(value any) []string
}

// decodeArguments reads the model's arguments, one JSON object; empty arguments are {}.
func decodeArguments(raw string) (map[string]any, []string) {
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || decoder.More() {
		return nil, []string{"arguments are not valid JSON"}
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, []string{fmt.Sprintf("Invalid input: expected object, received %s", jsTypeOf(value))}
	}
	return object, nil
}

// objectSchema checks an argument object: every field, and no member it does not declare.
func objectSchema(fields ...argumentField) func(map[string]any) []string {
	return func(arguments map[string]any) []string {
		var problems []string
		known := map[string]bool{}
		for _, field := range fields {
			known[field.name] = true
			value, present := arguments[field.name]
			if !present {
				if !field.optional {
					problems = append(problems, field.check(nil)...)
				}
				continue
			}
			problems = append(problems, field.check(value)...)
		}
		var unknown []string
		for name := range arguments {
			if !known[name] {
				unknown = append(unknown, name)
			}
		}
		sort.Strings(unknown)
		for _, name := range unknown {
			problems = append(problems, fmt.Sprintf("Unrecognized key: %s", jsJSONString(name)))
		}
		return problems
	}
}

// stringField is z.string() with optional .min(minimum) and .max(maximum); zero means no bound.
func stringField(name string, minimum, maximum int) argumentField {
	return argumentField{name: name, check: stringCheck(minimum, maximum)}
}

func stringCheck(minimum, maximum int) func(any) []string {
	return func(value any) []string {
		text, ok := value.(string)
		if !ok {
			return []string{fmt.Sprintf("Invalid input: expected string, received %s", jsTypeOf(value))}
		}
		length := utf16Length(text)
		if minimum > 0 && length < minimum {
			return []string{fmt.Sprintf("Too small: expected string to have >=%d characters", minimum)}
		}
		if maximum > 0 && length > maximum {
			return []string{fmt.Sprintf("Too big: expected string to have <=%d characters", maximum)}
		}
		return nil
	}
}

// enumField is z.enum(values).
func enumField(name string, values ...string) argumentField {
	return argumentField{name: name, check: func(value any) []string {
		text, _ := value.(string)
		for _, allowed := range values {
			if value != nil && text == allowed {
				return nil
			}
		}
		quoted := make([]string, len(values))
		for index, allowed := range values {
			quoted[index] = jsJSONString(allowed)
		}
		return []string{fmt.Sprintf("Invalid option: expected one of %s", strings.Join(quoted, "|"))}
	}}
}

// stringArrayField is z.array(z.string().min(itemMinimum).max(itemMaximum)).min(minimum).max(maximum).
func stringArrayField(name string, minimum, maximum, itemMinimum, itemMaximum int) argumentField {
	item := stringCheck(itemMinimum, itemMaximum)
	return argumentField{name: name, check: func(value any) []string {
		items, ok := value.([]any)
		if !ok {
			return []string{fmt.Sprintf("Invalid input: expected array, received %s", jsTypeOf(value))}
		}
		var problems []string
		for _, entry := range items {
			problems = append(problems, item(entry)...)
		}
		if len(items) < minimum {
			problems = append(problems, fmt.Sprintf("Too small: expected array to have >=%d items", minimum))
		}
		if maximum > 0 && len(items) > maximum {
			problems = append(problems, fmt.Sprintf("Too big: expected array to have <=%d items", maximum))
		}
		return problems
	}}
}

// jsTypeOf names a decoded JSON value as zod does.
func jsTypeOf(value any) string {
	switch value.(type) {
	case nil:
		return "undefined"
	case string:
		return "string"
	case json.Number, float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	default:
		return "object"
	}
}

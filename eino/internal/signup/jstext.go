package signup

// Text helpers that behave as the TypeScript application's string operations do, so texts and limits stay the same:
// JavaScript's \s and trim(), String.prototype.slice on UTF-16 code units, spread into code points, and JSON.stringify.

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf16"
)

// jsWhitespaceClass is JavaScript's \s: white space and line terminators.
const jsWhitespaceClass = `[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

var (
	jsWhitespaceRun = regexp.MustCompile(jsWhitespaceClass + `+`)
	jsWhitespaceOne = regexp.MustCompile(jsWhitespaceClass)
	slackFormatting = regexp.MustCompile("[`*_~]")
)

func isJSWhitespace(character rune) bool {
	switch character {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return character >= 0x2000 && character <= 0x200a
}

// jsTrim is String.prototype.trim.
func jsTrim(text string) string { return strings.TrimFunc(text, isJSWhitespace) }

// jsTrimEnd is String.prototype.trimEnd.
func jsTrimEnd(text string) string { return strings.TrimRightFunc(text, isJSWhitespace) }

// collapseWhitespace is text.replace(/\s+/g, " ").
func collapseWhitespace(text string) string { return jsWhitespaceRun.ReplaceAllString(text, " ") }

// cutUTF16 is text.slice(0, limit): the first limit UTF-16 code units. A surrogate pair the cut would split is left
// out, since Go text cannot hold half of it.
func cutUTF16(text string, limit int) string {
	units := 0
	for index, character := range text {
		size := utf16.RuneLen(character)
		if size < 0 {
			size = 1
		}
		if units+size > limit {
			return text[:index]
		}
		units += size
	}
	return text
}

// utf16Length is text.length.
func utf16Length(text string) int {
	length := 0
	for _, character := range text {
		if size := utf16.RuneLen(character); size > 0 {
			length += size
		} else {
			length++
		}
	}
	return length
}

// jsJSONString is JSON.stringify of one string.
func jsJSONString(text string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, character := range text {
		switch character {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if character < 0x20 {
				builder.WriteString(`\u00`)
				builder.WriteByte("0123456789abcdef"[character>>4])
				builder.WriteByte("0123456789abcdef"[character&0xf])
			} else {
				builder.WriteRune(character)
			}
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// marshalJS encodes value as JSON.stringify would for the plain values this application returns: no HTML escaping,
// members in declaration order.
func marshalJS(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}

// jsonMember is one member of an ordered JSON object.
type jsonMember struct {
	key   string
	value *string
}

// orderedJSONIndented writes an object of string or null members as JSON.stringify(value, null, 2) does.
func orderedJSONIndented(members []jsonMember) string {
	var builder strings.Builder
	builder.WriteString("{\n")
	for index, member := range members {
		builder.WriteString("  ")
		builder.WriteString(jsJSONString(member.key))
		builder.WriteString(": ")
		if member.value == nil {
			builder.WriteString("null")
		} else {
			builder.WriteString(jsJSONString(*member.value))
		}
		if index < len(members)-1 {
			builder.WriteByte(',')
		}
		builder.WriteByte('\n')
	}
	builder.WriteString("}")
	return builder.String()
}

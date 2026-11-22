package main

import "strings"

// parseDotEnv reads .env text as Node's --env-file does (Node 22, src/node_dotenv.cc Dotenv::ParseContent), so a
// .env that works for npm start gives this process the same values: a value in double, single or backtick quotes is
// the text between the first pair, with \n expanded in double quotes, and may span lines; an unquoted value ends at
// the first #; a later line of the same name wins. Its quirks are Node's, such as a key "export K" for "export K=".
func parseDotEnv(input string) map[string]string {
	values := map[string]string{}
	content := trimDotEnvSpaces(strings.ReplaceAll(input, "\r", ""))
	for content != "" {
		// An empty line or a comment.
		if content[0] == '\n' || content[0] == '#' {
			content = afterNewline(content, 0)
			continue
		}
		equalOrNewline := strings.IndexAny(content, "=\n")
		if equalOrNewline < 0 || content[equalOrNewline] == '\n' {
			// A line without =.
			if strings.IndexByte(content, '\n') < 0 {
				break
			}
			content = trimDotEnvSpaces(afterNewline(content, 0))
			continue
		}
		key := trimDotEnvSpaces(content[:equalOrNewline])
		content = content[equalOrNewline+1:]
		if content == "" || content[0] == '\n' {
			values[key] = ""
			continue
		}
		content = trimDotEnvSpaces(content)
		if key == "" {
			// Node goes on after the =, so the rest of the line is read again.
			continue
		}
		if strings.HasPrefix(key, "export ") {
			key = trimDotEnvSpaces(key[len("export "):])
		}
		if content == "" {
			values[key] = ""
			break
		}
		quote := content[0]
		if quote == '"' || quote == '\'' || quote == '`' {
			closing := strings.IndexByte(content[1:], quote)
			if closing >= 0 {
				closing++
				value := content[1:closing]
				if quote == '"' {
					value = strings.ReplaceAll(value, `\n`, "\n")
				}
				values[key] = value
				content = afterNewline(content, closing+1)
				continue
			}
			// No closing quote: the rest of the line, the quote included.
			newline := strings.IndexByte(content, '\n')
			if newline < 0 {
				values[key] = content
				break
			}
			values[key] = content[:newline]
			content = trimDotEnvSpaces(content[newline:])
			continue
		}
		line := content
		if newline := strings.IndexByte(content, '\n'); newline >= 0 {
			line = content[:newline]
			content = content[newline+1:]
		} else {
			content = ""
		}
		if hash := strings.IndexByte(line, '#'); hash >= 0 {
			line = line[:hash]
		}
		values[key] = trimDotEnvSpaces(line)
		content = trimDotEnvSpaces(content)
	}
	return values
}

// afterNewline is content after the first newline at or after from, or "" without one.
func afterNewline(content string, from int) string {
	newline := strings.IndexByte(content[from:], '\n')
	if newline < 0 {
		return ""
	}
	return content[from+newline+1:]
}

// trimDotEnvSpaces trims the spaces, tabs and newlines Node's parser trims.
func trimDotEnvSpaces(text string) string {
	return strings.Trim(text, " \t\n")
}

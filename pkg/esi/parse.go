package esi

import (
	"bytes"
	"strings"
)

var endTagOpen = []byte("</esi:")

// entities are the ones XML predefines, the only ones an ESI attribute value
// may hold.
var entities = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'")

// startTag is the start tag of an ESI element, `<esi:name attr="value">`, or
// an empty element, `<esi:name attr="value"/>`.
type startTag struct {
	name   string
	attrs  map[string]string
	empty  bool
	length int
}

// parseStartTag parses the ESI start tag b starts with. It reports false when
// b doesn't start with a well-formed one.
func parseStartTag(b []byte) (startTag, bool) {
	if !bytes.HasPrefix(b, tagOpen) {
		return startTag{}, false
	}

	i := len(tagOpen)
	for i < len(b) && isNameByte(b[i]) {
		i++
	}

	tag := startTag{name: string(b[len(tagOpen):i]), attrs: map[string]string{}}
	if tag.name == "" || i >= len(b) || (!isSpace(b[i]) && b[i] != '>' && b[i] != '/') {
		return startTag{}, false
	}

	for {
		i = skipSpaces(b, i)
		if i >= len(b) {
			return startTag{}, false
		}

		switch b[i] {
		case '>':
			tag.length = i + 1

			return tag, true
		case '/':
			if i+1 < len(b) && b[i+1] == '>' {
				tag.empty, tag.length = true, i+2

				return tag, true
			}

			return startTag{}, false
		}

		var name, value string
		if name, value, i = parseAttribute(b, i); i < 0 {
			return startTag{}, false
		}

		if _, seen := tag.attrs[name]; !seen {
			tag.attrs[name] = value
		}
	}
}

// parseAttribute parses the attribute starting at b[i]. It returns its name,
// its value, and where it ends in b, or -1 when it is malformed. Unquoted and
// valueless attributes are accepted, as an HTML author would write them.
func parseAttribute(b []byte, i int) (string, string, int) {
	start := i
	for i < len(b) && !isSpace(b[i]) && b[i] != '=' && b[i] != '>' && b[i] != '/' {
		i++
	}

	name := strings.ToLower(string(b[start:i]))
	if name == "" {
		return "", "", -1
	}

	i = skipSpaces(b, i)
	if i >= len(b) || b[i] != '=' {
		return name, "", i
	}

	i = skipSpaces(b, i+1)
	if i >= len(b) {
		return "", "", -1
	}

	if quote := b[i]; quote == '"' || quote == '\'' {
		end := bytes.IndexByte(b[i+1:], quote)
		if end < 0 {
			return "", "", -1
		}

		return name, entities.Replace(string(b[i+1 : i+1+end])), i + end + 2
	}

	start = i
	for i < len(b) && !isSpace(b[i]) && b[i] != '>' && (b[i] != '/' || i+1 >= len(b) || b[i+1] != '>') {
		i++
	}

	return name, entities.Replace(string(b[start:i])), i
}

// endTagLength returns the length of the `</esi:name>` end tag b starts with,
// or 0 when it doesn't start with one.
func endTagLength(b []byte, name string) int {
	nameEnd := len(endTagOpen) + len(name)
	if !bytes.HasPrefix(b, endTagOpen) || len(b) < nameEnd || string(b[len(endTagOpen):nameEnd]) != name {
		return 0
	}

	i := skipSpaces(b, nameEnd)
	if i < len(b) && b[i] == '>' {
		return i + 1
	}

	return 0
}

// elementContent returns the content of the element whose start tag b starts
// with, and the length of the whole element in b. The elements of the same
// name nested in it are skipped over. It reports false when the element is
// missing its end tag.
func elementContent(b []byte, tag startTag) ([]byte, int, bool) {
	if tag.empty {
		return nil, tag.length, true
	}

	nested := 0
	for i := tag.length; i < len(b); {
		next := bytes.IndexByte(b[i:], '<')
		if next < 0 {
			break
		}
		i += next

		if n := endTagLength(b[i:], tag.name); n > 0 {
			if nested == 0 {
				return b[tag.length:i], i + n, true
			}

			nested--
			i += n

			continue
		}

		if inner, ok := parseStartTag(b[i:]); ok && inner.name == tag.name {
			if !inner.empty {
				nested++
			}
			i += inner.length

			continue
		}

		i++
	}

	return nil, 0, false
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func skipSpaces(b []byte, i int) int {
	for i < len(b) && isSpace(b[i]) {
		i++
	}

	return i
}

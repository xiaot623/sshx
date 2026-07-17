package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type propertySpan struct {
	valueStart int
	valueEnd   int
}

func StringProperty(src []byte, key string) (string, bool, error) {
	span, found, _, err := findProperty(src, key)
	if err != nil || !found {
		return "", found, err
	}
	var value string
	if err := json.Unmarshal(src[span.valueStart:span.valueEnd], &value); err != nil {
		return "", true, fmt.Errorf("%s must be a string", key)
	}
	return value, true, nil
}

func SetStringProperty(src []byte, key, value string) ([]byte, error) {
	span, found, closeBrace, err := findProperty(src, key)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(value)
	if found {
		out := make([]byte, 0, len(src)+len(encoded))
		out = append(out, src[:span.valueStart]...)
		out = append(out, encoded...)
		out = append(out, src[span.valueEnd:]...)
		return out, nil
	}
	if closeBrace < 0 {
		return nil, errors.New("settings root must be a JSON object")
	}
	prefix := src[:closeBrace]
	comma := ""
	last := lastSignificant(prefix)
	if last != '{' && last != ',' {
		comma = ","
	}
	indent := detectIndent(src)
	entry := comma + "\n" + indent + string(mustJSON(key)) + ": " + string(encoded) + "\n"
	out := make([]byte, 0, len(src)+len(entry))
	out = append(out, prefix...)
	out = append(out, entry...)
	out = append(out, src[closeBrace:]...)
	return out, nil
}

func lastSignificant(src []byte) byte {
	var last byte
	for i := 0; i < len(src); {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '"':
			last = '"'
			end, err := stringEnd(src, i)
			if err != nil {
				return last
			}
			i = end
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				i += 2
				for i < len(src) && src[i] != '\n' {
					i++
				}
				continue
			}
			if i+1 < len(src) && src[i+1] == '*' {
				i += 2
				for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
					i++
				}
				if i+1 < len(src) {
					i += 2
				}
				continue
			}
			last = src[i]
			i++
		default:
			last = src[i]
			i++
		}
	}
	return last
}

func findProperty(src []byte, wanted string) (propertySpan, bool, int, error) {
	i := skipTrivia(src, 0)
	if i >= len(src) || src[i] != '{' {
		return propertySpan{}, false, -1, errors.New("settings root must be a JSON object")
	}
	i++
	found := false
	var result propertySpan
	for {
		i = skipTrivia(src, i)
		if i >= len(src) {
			return propertySpan{}, false, -1, errors.New("unterminated settings object")
		}
		if src[i] == '}' {
			return result, found, i, nil
		}
		if src[i] == ',' {
			i++
			continue
		}
		if src[i] != '"' {
			return propertySpan{}, false, -1, fmt.Errorf("expected settings property at byte %d", i)
		}
		keyEnd, err := stringEnd(src, i)
		if err != nil {
			return propertySpan{}, false, -1, err
		}
		var key string
		if err := json.Unmarshal(src[i:keyEnd], &key); err != nil {
			return propertySpan{}, false, -1, err
		}
		i = skipTrivia(src, keyEnd)
		if i >= len(src) || src[i] != ':' {
			return propertySpan{}, false, -1, fmt.Errorf("expected colon after %q", key)
		}
		valueStart := skipTrivia(src, i+1)
		valueEnd, err := valueEnd(src, valueStart)
		if err != nil {
			return propertySpan{}, false, -1, err
		}
		if key == wanted {
			if found {
				return propertySpan{}, false, -1, fmt.Errorf("duplicate %s setting", wanted)
			}
			found = true
			result = propertySpan{valueStart: valueStart, valueEnd: valueEnd}
		}
		i = valueEnd
	}
}

func valueEnd(src []byte, start int) (int, error) {
	if start >= len(src) {
		return 0, errors.New("missing settings value")
	}
	if src[start] == '"' {
		return stringEnd(src, start)
	}
	depth := 0
	for i := start; i < len(src); {
		switch src[i] {
		case '"':
			end, err := stringEnd(src, i)
			if err != nil {
				return 0, err
			}
			i = end
			continue
		case '/':
			if i+1 < len(src) && (src[i+1] == '/' || src[i+1] == '*') {
				i = skipTrivia(src, i)
				continue
			}
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return trimEnd(src, start, i), nil
			}
			depth--
		case ',':
			if depth == 0 {
				return trimEnd(src, start, i), nil
			}
		}
		i++
	}
	return 0, errors.New("unterminated settings value")
}

func stringEnd(src []byte, start int) (int, error) {
	for i := start + 1; i < len(src); i++ {
		if src[i] == '\\' {
			i++
			continue
		}
		if src[i] == '"' {
			return i + 1, nil
		}
	}
	return 0, errors.New("unterminated JSON string")
}

func skipTrivia(src []byte, start int) int {
	i := start
	for i < len(src) {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '/':
			if i+1 >= len(src) {
				return i
			}
			if src[i+1] == '/' {
				i += 2
				for i < len(src) && src[i] != '\n' {
					i++
				}
				continue
			}
			if src[i+1] == '*' {
				i += 2
				for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
					i++
				}
				if i+1 < len(src) {
					i += 2
				}
				continue
			}
			return i
		default:
			return i
		}
	}
	return i
}

func trimEnd(src []byte, start, end int) int {
	for end > start {
		switch src[end-1] {
		case ' ', '\t', '\r', '\n':
			end--
		default:
			return end
		}
	}
	return end
}

func detectIndent(src []byte) string {
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "\"") {
			return line[:len(line)-len(trimmed)]
		}
	}
	return "  "
}

func mustJSON(value string) []byte {
	b, _ := json.Marshal(value)
	return b
}

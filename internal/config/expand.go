package config

import (
	"fmt"
	"strings"
)

// expand replaces ${VAR} and ${VAR:-default} placeholders in s.
//
// ${VAR} requires VAR to be set (an empty value is allowed); ${VAR:-default}
// falls back to default when VAR is unset or empty, mirroring POSIX shells.
// Names of required but unset variables are returned in missing so the caller
// can report all of them at once.
func expand(s string, lookup LookupFunc) (out string, missing []string, err error) {
	if !strings.Contains(s, "${") {
		return s, nil, nil
	}

	var b strings.Builder
	rest := s
	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:start])

		end := strings.IndexByte(rest[start:], '}')
		if end < 0 {
			return "", nil, fmt.Errorf("unterminated placeholder in %q", s)
		}
		expr := rest[start+2 : start+end]
		rest = rest[start+end+1:]

		name, def, hasDefault := strings.Cut(expr, ":-")
		if !validName(name) {
			return "", nil, fmt.Errorf("invalid variable name %q in %q", name, s)
		}

		val, ok := lookup(name)
		switch {
		case hasDefault && val == "":
			b.WriteString(def)
		case ok:
			b.WriteString(val)
		default:
			missing = append(missing, name)
		}
	}
	return b.String(), missing, nil
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

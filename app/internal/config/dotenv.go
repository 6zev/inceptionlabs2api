// Package-level .env support: the gateway ships as a single static binary and
// has no CLI flags, so operators otherwise have to `source` a file by hand.
// Reading one small, well-defined format keeps that convenient without adding
// a dependency (the module is deliberately stdlib-only).
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// DotEnvFileEnv names the variable holding an explicit .env path. When it is
// set the file MUST exist (an operator who names a file wants to know it was
// not read); the default lookup is best-effort.
const DotEnvFileEnv = "IL2API_ENV_FILE"

// defaultDotEnvFiles are tried in order when no explicit path is configured.
// ".env" first, since that is what docker compose and most tooling use.
var defaultDotEnvFiles = []string{".env"}

// LoadDotEnv reads a .env file and fills every unset variable in the process
// environment. It returns the path actually read ("" when none was found and
// none was required) and whether anything was read.
//
// Precedence: a variable already present in the process environment always
// wins, so `IL2API_ADDR=:4000 ./inceptionlabs2api` still overrides the file,
// and systemd EnvironmentFile behaviour is not silently reversed. This mirrors
// `set -a; source .env` only when the environment is empty, which is the
// common case, and is the safer of the two on a shared host.
func LoadDotEnv() (path string, err error) {
	explicit := strings.TrimSpace(os.Getenv(DotEnvFileEnv))
	if explicit != "" {
		if err := loadDotEnvFile(explicit); err != nil {
			return explicit, err
		}
		return explicit, nil
	}
	for _, candidate := range defaultDotEnvFiles {
		st, statErr := os.Stat(candidate)
		if statErr != nil {
			continue
		}
		if st.IsDir() {
			return candidate, fmt.Errorf("%s is a directory, not a .env file", candidate)
		}
		if err := loadDotEnvFile(candidate); err != nil {
			return candidate, err
		}
		return candidate, nil
	}
	return "", nil
}

// loadDotEnvFile parses one file and sets the variables it names that are not
// already set. A file that cannot be opened is an error; a malformed line is
// an error too, because silently ignoring a typo in a secret is worse than
// refusing to start.
func loadDotEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s: file not found (unset %s to use the default lookup)", path, DotEnvFileEnv)
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// A line is a single small assignment; 1 MiB is far beyond any real value
	// (the only large ones are timeouts and sizes) and bounds hostile input.
	scanner.Buffer(make([]byte, 64*1024), 1<<20)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		key, value, ok, err := parseDotEnvLine(scanner.Text())
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue // the process environment wins
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s:%d: set %s: %w", path, lineNo, key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// parseDotEnvLine splits one line into a key and a value. ok is false for
// blank lines and comments. The accepted syntax is the useful subset of what
// shell dotenv files contain:
//
//	KEY=value            bare
//	KEY="value"          double quoted: \n \t \" \\ escapes, # is literal
//	KEY='value'          single quoted: fully literal
//	export KEY=value     leading `export` is ignored
//	KEY=value # comment  trailing comment after unquoted or double-quoted text
//
// Unquoted values are trimmed of surrounding whitespace; quoted values keep
// theirs, since leading spaces inside quotes are usually intentional.
func parseDotEnvLine(raw string) (key, value string, ok bool, err error) {
	line := strings.TrimLeft(raw, " \t")
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	// Tolerate a UTF-8 BOM on the first line of files edited on Windows.
	line = strings.TrimPrefix(line, "\ufeff")
	if strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "export\t") {
		line = strings.TrimLeft(line[len("export"):], " \t")
	}

	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", false, fmt.Errorf("expected KEY=value, got %q", strings.TrimSpace(raw))
	}
	key = strings.TrimSpace(line[:eq])
	if key == "" {
		return "", "", false, fmt.Errorf("empty key in %q", strings.TrimSpace(raw))
	}
	if !isValidDotEnvKey(key) {
		return "", "", false, fmt.Errorf("invalid key %q (letters, digits and _ only)", key)
	}

	rest := strings.TrimLeft(line[eq+1:], " \t")
	switch {
	case strings.HasPrefix(rest, `"`):
		value, err = parseDoubleQuoted(rest[1:])
	case strings.HasPrefix(rest, "'"):
		value, err = parseSingleQuoted(rest[1:])
	default:
		value = stripInlineComment(rest)
		value = strings.TrimSpace(value)
	}
	if err != nil {
		return "", "", false, err
	}
	return key, value, true, nil
}

// parseDoubleQuoted reads a "..." value, honouring \n \r \t \" \\ and passing
// any other escape through unchanged. Text after the closing quote is allowed
// only if it is a comment.
func parseDoubleQuoted(s string) (string, error) {
	var b strings.Builder
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			switch c {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				// Unknown escape: keep it verbatim rather than dropping the
				// backslash, so a Windows path in a value survives.
				b.WriteByte('\\')
				b.WriteByte(c)
			}
			escaped = false
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '"':
			if err := trailingCommentOnly(s[i+1:]); err != nil {
				return "", err
			}
			return b.String(), nil
		default:
			b.WriteByte(c)
		}
	}
	return "", fmt.Errorf("unterminated double-quoted value")
}

// parseSingleQuoted reads a '...' value. Nothing is interpreted inside single
// quotes, matching shell behaviour.
func parseSingleQuoted(s string) (string, error) {
	end := strings.IndexByte(s, '\'')
	if end < 0 {
		return "", fmt.Errorf("unterminated single-quoted value")
	}
	if err := trailingCommentOnly(s[end+1:]); err != nil {
		return "", err
	}
	return s[:end], nil
}

// trailingCommentOnly accepts an empty remainder or a comment.
func trailingCommentOnly(s string) error {
	rest := strings.TrimSpace(s)
	if rest == "" || strings.HasPrefix(rest, "#") {
		return nil
	}
	return fmt.Errorf("unexpected text after quoted value: %q", rest)
}

// stripInlineComment removes a trailing " # comment" from an unquoted value.
// A '#' only starts a comment at the beginning of the value or after
// whitespace, so a value like `abc#def` keeps its hash.
func stripInlineComment(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != '#' {
			continue
		}
		if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
			return s[:i]
		}
	}
	return s
}

// isValidDotEnvKey reports whether key is an environment variable name that
// os.Setenv will accept without surprising the caller.
func isValidDotEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

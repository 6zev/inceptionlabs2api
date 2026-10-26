package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return path
}

// clearEnv unsets the given keys and restores them when the test ends.
func clearEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if old, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { os.Setenv(k, old) })
		} else {
			t.Cleanup(func() { os.Unsetenv(k) })
		}
		os.Unsetenv(k)
	}
}

func TestParseDotEnvLine(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		key   string
		value string
		ok    bool
	}{
		{"bare", "A=1", "A", "1", true},
		{"spaces around equals", "A = 1", "A", "1", true},
		{"empty value", "A=", "A", "", true},
		{"comment line", "# nope", "", "", false},
		{"blank", "   ", "", "", false},
		{"export prefix", "export A=1", "A", "1", true},
		{"double quoted keeps spaces", `A="  hi there  "`, "A", "  hi there  ", true},
		{"double quoted escapes", `A="line1\nline2\ttab"`, "A", "line1\nline2\ttab", true},
		{"double quoted escaped quote", `A="say \"hi\""`, "A", `say "hi"`, true},
		{"single quoted literal", `A='\n not a newline'`, "A", `\n not a newline`, true},
		{"trailing comment unquoted", "A=1 # note", "A", "1", true},
		{"trailing comment quoted", `A="1" # note`, "A", "1", true},
		{"hash inside value", "A=abc#def", "A", "abc#def", true},
		{"hash after space is comment", "A=abc #def", "A", "abc", true},
		{"bom tolerated", "\ufeffA=1", "A", "1", true},
		{"windows path in quotes", `A="C:\Users\x"`, "A", `C:\Users\x`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, value, ok, err := parseDotEnvLine(c.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if key != c.key || value != c.value {
				t.Fatalf("got %q=%q, want %q=%q", key, value, c.key, c.value)
			}
		})
	}
}

func TestParseDotEnvLineErrors(t *testing.T) {
	for _, in := range []string{
		"no equals sign",
		`A="unterminated`,
		"A='unterminated",
		`A="v" trailing`,
		"=novalue",
		"BAD KEY=1",
	} {
		if _, _, _, err := parseDotEnvLine(in); err == nil {
			t.Errorf("parseDotEnvLine(%q) = nil error, want an error", in)
		}
	}
}

func TestLoadDotEnvExplicitFile(t *testing.T) {
	clearEnv(t, "IL2API_ENV_FILE", "IL2API_DOTENV_TEST_A", "IL2API_DOTENV_TEST_B")
	path := writeEnv(t, "IL2API_DOTENV_TEST_A=hello\nIL2API_DOTENV_TEST_B=\"two words\"\n")

	t.Setenv("IL2API_ENV_FILE", path)
	got, err := LoadDotEnv()
	if err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
	if v := os.Getenv("IL2API_DOTENV_TEST_A"); v != "hello" {
		t.Errorf("A = %q, want hello", v)
	}
	if v := os.Getenv("IL2API_DOTENV_TEST_B"); v != "two words" {
		t.Errorf("B = %q, want 'two words'", v)
	}
}

// The process environment must win: an operator exporting a variable to
// override the file is the documented contract.
func TestLoadDotEnvProcessEnvWins(t *testing.T) {
	clearEnv(t, "IL2API_DOTENV_TEST_A")
	t.Setenv("IL2API_DOTENV_TEST_A", "from-process")
	path := writeEnv(t, "IL2API_DOTENV_TEST_A=from-file\n")
	t.Setenv("IL2API_ENV_FILE", path)

	if _, err := LoadDotEnv(); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if v := os.Getenv("IL2API_DOTENV_TEST_A"); v != "from-process" {
		t.Fatalf("A = %q, want the process value", v)
	}
}

// An explicitly named file that is missing is an error, never a silent no-op.
func TestLoadDotEnvExplicitMissingFileErrors(t *testing.T) {
	t.Setenv("IL2API_ENV_FILE", filepath.Join(t.TempDir(), "nope.env"))
	if _, err := LoadDotEnv(); err == nil {
		t.Fatal("expected an error for a missing explicit .env")
	}
}

// A malformed line must refuse the load rather than silently drop a value.
func TestLoadDotEnvMalformedLineErrors(t *testing.T) {
	clearEnv(t, "IL2API_DOTENV_TEST_A")
	path := writeEnv(t, "IL2API_DOTENV_TEST_A=ok\nthis line is broken\n")
	t.Setenv("IL2API_ENV_FILE", path)
	if _, err := LoadDotEnv(); err == nil {
		t.Fatal("expected an error for a malformed line")
	}
}

// With no IL2API_ENV_FILE and no .env in the working directory, the lookup is
// a silent no-op.
func TestLoadDotEnvNoFileIsNoop(t *testing.T) {
	clearEnv(t, "IL2API_ENV_FILE")
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(old) })

	got, err := LoadDotEnv()
	if err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got != "" {
		t.Fatalf("path = %q, want empty", got)
	}
}

// The default lookup picks up ./.env without any configuration.
func TestLoadDotEnvDefaultLookup(t *testing.T) {
	clearEnv(t, "IL2API_ENV_FILE", "IL2API_DOTENV_TEST_A")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("IL2API_DOTENV_TEST_A=default\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(old) })

	got, err := LoadDotEnv()
	if err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got != ".env" {
		t.Fatalf("path = %q, want .env", got)
	}
	if v := os.Getenv("IL2API_DOTENV_TEST_A"); v != "default" {
		t.Fatalf("A = %q, want default", v)
	}
}

// End to end: a .env value actually reaches the parsed Config.
func TestLoadReadsDotEnvIntoConfig(t *testing.T) {
	clearEnv(t, "IL2API_ENV_FILE", "IL2API_DOTENV_TEST_MODEL")
	path := writeEnv(t, "IL2API_MODEL=mercury-test-from-env\n")
	t.Setenv("IL2API_ENV_FILE", path)
	// The helper above only clears the test key; the model must be unset so
	// the file can supply it.
	oldModel, hadModel := os.LookupEnv("IL2API_MODEL")
	os.Unsetenv("IL2API_MODEL")
	t.Cleanup(func() {
		if hadModel {
			os.Setenv("IL2API_MODEL", oldModel)
		} else {
			os.Unsetenv("IL2API_MODEL")
		}
	})

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Model != "mercury-test-from-env" {
		t.Fatalf("Model = %q, want the .env value", c.Model)
	}
	if c.Models[0] != "mercury-test-from-env" {
		t.Fatalf("Models = %v, want the .env value", c.Models)
	}
}

func TestStripInlineComment(t *testing.T) {
	cases := map[string]string{
		"1 # note":   "1 ",
		"abc#def":    "abc#def",
		"# whole":    "",
		"a b # tail": "a b ",
	}
	for in, want := range cases {
		if got := stripInlineComment(in); got != want {
			t.Errorf("stripInlineComment(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsValidDotEnvKey(t *testing.T) {
	good := []string{"A", "_A", "IL2API_MODEL", "A1", "a_b_c"}
	for _, k := range good {
		if !isValidDotEnvKey(k) {
			t.Errorf("isValidDotEnvKey(%q) = false, want true", k)
		}
	}
	bad := []string{"", "1A", "A-B", "A B", "A.B", strings.Repeat("A", 3) + "\n"}
	for _, k := range bad {
		if isValidDotEnvKey(k) {
			t.Errorf("isValidDotEnvKey(%q) = true, want false", k)
		}
	}
}

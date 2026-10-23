package config

import "testing"

func TestNativeWebSearchDefaultsOn(t *testing.T) {
	// Arrange: no IL2API_NATIVE_WEB_SEARCH in the environment.
	getenv := func(string) string { return "" }

	// Act
	c, err := FromEnv(getenv)

	// Assert
	if err != nil {
		t.Fatalf("FromEnv: unexpected error: %v", err)
	}
	if !c.NativeWebSearch {
		t.Errorf("NativeWebSearch default = false, want true")
	}
}

func TestNativeWebSearchOffEnvDisables(t *testing.T) {
	// Arrange: explicit off must still win over the default.
	getenv := func(key string) string {
		if key == "IL2API_NATIVE_WEB_SEARCH" {
			return "off"
		}
		return ""
	}

	// Act
	c, err := FromEnv(getenv)

	// Assert
	if err != nil {
		t.Fatalf("FromEnv: unexpected error: %v", err)
	}
	if c.NativeWebSearch {
		t.Errorf("NativeWebSearch with IL2API_NATIVE_WEB_SEARCH=off = true, want false")
	}
}

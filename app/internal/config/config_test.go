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

func TestNewLimitsDefaults(t *testing.T) {
	c, err := FromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.MaxMessages != 96 {
		t.Errorf("MaxMessages default = %d, want 96", c.MaxMessages)
	}
	if c.EmptyRetries != 1 {
		t.Errorf("EmptyRetries default = %d, want 1", c.EmptyRetries)
	}
	if c.SessionPerRequest {
		t.Errorf("SessionPerRequest default = true, want false")
	}
}

func TestNewLimitsFromEnv(t *testing.T) {
	env := map[string]string{
		"IL2API_MAX_MESSAGES":        "64",
		"IL2API_EMPTY_RETRIES":       "3",
		"IL2API_SESSION_PER_REQUEST": "on",
	}
	c, err := FromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.MaxMessages != 64 || c.EmptyRetries != 3 || !c.SessionPerRequest {
		t.Fatalf("got MaxMessages=%d EmptyRetries=%d SessionPerRequest=%v", c.MaxMessages, c.EmptyRetries, c.SessionPerRequest)
	}
}

func TestMaxMessagesZeroDisablesCap(t *testing.T) {
	c, err := FromEnv(func(k string) string {
		if k == "IL2API_MAX_MESSAGES" {
			return "0"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("0 must be accepted as 'cap off': %v", err)
	}
	if c.MaxMessages != 0 {
		t.Fatalf("MaxMessages = %d, want 0", c.MaxMessages)
	}
}

func TestMaxMessagesTooSmallRejected(t *testing.T) {
	for _, v := range []string{"1", "3"} {
		_, err := FromEnv(func(k string) string {
			if k == "IL2API_MAX_MESSAGES" {
				return v
			}
			return ""
		})
		if err == nil {
			t.Errorf("IL2API_MAX_MESSAGES=%s must be rejected (min 4 or 0)", v)
		}
	}
}

func TestEmptyRetriesNegativeRejected(t *testing.T) {
	_, err := FromEnv(func(k string) string {
		if k == "IL2API_EMPTY_RETRIES" {
			return "-1"
		}
		return ""
	})
	if err == nil {
		t.Error("negative IL2API_EMPTY_RETRIES must be rejected")
	}
}

func TestSessionPerRequestInvalidRejected(t *testing.T) {
	_, err := FromEnv(func(k string) string {
		if k == "IL2API_SESSION_PER_REQUEST" {
			return "maybe"
		}
		return ""
	})
	if err == nil {
		t.Error("invalid IL2API_SESSION_PER_REQUEST must be rejected")
	}
}

// The default model surface is one name. Widening IL2API_MODELS does not
// unlock a different backend (the upstream ignores the model field), so the
// default must stay narrow rather than shipping speculative aliases.
func TestDefaultModelsIsSingleMercury3(t *testing.T) {
	c, err := FromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.Model != "mercury-3" {
		t.Errorf("Model default = %q, want mercury-3", c.Model)
	}
	if len(c.Models) != 1 || c.Models[0] != "mercury-3" {
		t.Errorf("Models default = %v, want [mercury-3]", c.Models)
	}
}

func TestModelsAllowlistFromEnv(t *testing.T) {
	c, err := FromEnv(func(k string) string {
		if k == "IL2API_MODELS" {
			return "mercury-3, mercury-4"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if len(c.Models) != 2 || c.Models[1] != "mercury-4" {
		t.Fatalf("Models = %v, want both listed names", c.Models)
	}
}

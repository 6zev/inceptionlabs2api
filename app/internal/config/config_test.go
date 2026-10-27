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

// The default model surface is one real name plus its "-thinking" alias.
// Widening IL2API_MODELS does not unlock a different backend (the upstream
// ignores the model field), so the real surface must stay narrow: the alias is
// a label for a reasoning-aware client, never a second backend. See
// ThinkingSuffix for the measured client (Cherry Studio) this serves.
func TestDefaultModelsIsSingleMercury3(t *testing.T) {
	c, err := FromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if c.Model != "mercury-3" {
		t.Errorf("Model default = %q, want mercury-3", c.Model)
	}
	want := []string{"mercury-3", "mercury-3" + ThinkingSuffix}
	if len(c.Models) != len(want) {
		t.Fatalf("Models default = %v, want %v", c.Models, want)
	}
	for i := range want {
		if c.Models[i] != want[i] {
			t.Errorf("Models[%d] = %q, want %q", i, c.Models[i], want[i])
		}
	}
}

// TestThinkingAliasIsDerivedFromNames pins the alias rule: each non-reasoning
// name gains one suffix, a name that already advertises reasoning gains none,
// and an explicitly listed alias is not duplicated.
func TestThinkingAliasIsDerivedFromNames(t *testing.T) {
	c, err := FromEnv(func(k string) string {
		switch k {
		case "IL2API_MODELS":
			return "mercury-3, mercury-thinking, mercury-3-thinking"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	want := []string{"mercury-3", "mercury-thinking", "mercury-3-thinking"}
	if len(c.Models) != len(want) {
		t.Fatalf("Models = %v, want %v (explicit alias must not duplicate)", c.Models, want)
	}
	for i := range want {
		if c.Models[i] != want[i] {
			t.Errorf("Models[%d] = %q, want %q", i, c.Models[i], want[i])
		}
	}
}

// TestThinkingAliasFollowsRenamedModel covers IL2API_MODEL=foo: the alias must
// follow the configured name rather than being hard-coded to mercury-3.
func TestThinkingAliasFollowsRenamedModel(t *testing.T) {
	c, err := FromEnv(func(k string) string {
		if k == "IL2API_MODEL" {
			return "custom-model"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	want := []string{"custom-model", "custom-model" + ThinkingSuffix}
	if len(c.Models) != len(want) {
		t.Fatalf("Models = %v, want %v", c.Models, want)
	}
	for i := range want {
		if c.Models[i] != want[i] {
			t.Errorf("Models[%d] = %q, want %q", i, c.Models[i], want[i])
		}
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
	// Operator names keep their order at the front; each gains a thinking
	// alias afterwards. Both listed names must still be accepted.
	want := []string{"mercury-3", "mercury-4", "mercury-3-thinking", "mercury-4-thinking"}
	if len(c.Models) != len(want) {
		t.Fatalf("Models = %v, want %v", c.Models, want)
	}
	for i := range want {
		if c.Models[i] != want[i] {
			t.Errorf("Models[%d] = %q, want %q", i, c.Models[i], want[i])
		}
	}
	// The accept-list must still accept what the operator wrote.
	for _, listed := range []string{"mercury-3", "mercury-4"} {
		found := false
		for _, m := range c.Models {
			if m == listed {
				found = true
			}
		}
		if !found {
			t.Errorf("%q missing from the accept-list", listed)
		}
	}
}

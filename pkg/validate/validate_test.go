package validate

import (
	"strings"
	"testing"
)

func TestName(t *testing.T) {
	valid := []string{"", "my-session", "test_123", "Session.v2", "a"}
	for _, n := range valid {
		if err := Name(n); err != nil {
			t.Errorf("Name(%q) should be valid, got: %v", n, err)
		}
	}

	invalid := []string{"-start-dash", ".dot", " space", "a/b", string(make([]byte, 200))}
	for _, n := range invalid {
		if err := Name(n); err == nil {
			t.Errorf("Name(%q) should be invalid", n)
		}
	}
}

func TestName_MaxBoundary(t *testing.T) {
	// The pattern allows up to 128 chars total: 1 leading + up to 127 more.
	valid128 := "a" + strings.Repeat("b", 127)
	if err := Name(valid128); err != nil {
		t.Errorf("Name with 128 chars should be valid, got: %v", err)
	}

	invalid129 := "a" + strings.Repeat("b", 128)
	if err := Name(invalid129); err == nil {
		t.Error("Name with 129 chars should be invalid")
	}
}

func TestName_Unicode(t *testing.T) {
	// Unicode characters are not in [a-zA-Z0-9._-], so they should be rejected.
	unicodeNames := []string{"cafe\u0301", "\u00e9mile", "\u4e16\u754c", "hello\u2603"}
	for _, n := range unicodeNames {
		if err := Name(n); err == nil {
			t.Errorf("Name(%q) with unicode should be invalid", n)
		}
	}
}

func TestName_SingleChar(t *testing.T) {
	// Single alphanumeric char is valid.
	for _, c := range []string{"a", "Z", "0", "9"} {
		if err := Name(c); err != nil {
			t.Errorf("Name(%q) should be valid, got: %v", c, err)
		}
	}
	// Single special char is invalid (must start with alphanumeric).
	for _, c := range []string{".", "-", "_"} {
		if err := Name(c); err == nil {
			t.Errorf("Name(%q) should be invalid", c)
		}
	}
}

func TestMode(t *testing.T) {
	for _, m := range []string{"explore", "execute", "ask", ""} {
		if err := Mode(m); err != nil {
			t.Errorf("Mode(%q) should be valid", m)
		}
	}
	if err := Mode("invalid"); err == nil {
		t.Error("Mode(invalid) should fail")
	}
}

func TestMode_EdgeCases(t *testing.T) {
	invalid := []string{"Explore", "EXECUTE", "ASK", " explore", "execute ", "explore\n"}
	for _, m := range invalid {
		if err := Mode(m); err == nil {
			t.Errorf("Mode(%q) should be invalid (case/whitespace sensitive)", m)
		}
	}
}

func TestPackageName(t *testing.T) {
	if err := PackageName("python@3.12"); err != nil {
		t.Errorf("PackageName should accept version spec: %v", err)
	}
	if err := PackageName(""); err == nil {
		t.Error("PackageName should reject empty")
	}
}

func TestPackageName_LongName(t *testing.T) {
	// Exactly 64 chars should be valid.
	name64 := strings.Repeat("x", 64)
	if err := PackageName(name64); err != nil {
		t.Errorf("PackageName with 64 chars should be valid, got: %v", err)
	}

	// 65 chars should be invalid.
	name65 := strings.Repeat("x", 65)
	if err := PackageName(name65); err == nil {
		t.Error("PackageName with 65 chars should be invalid")
	}
}

func TestPackageName_WithVersion(t *testing.T) {
	// Version part should not affect name length check.
	name64 := strings.Repeat("x", 64)
	if err := PackageName(name64 + "@1.0.0"); err != nil {
		t.Errorf("PackageName with 64-char name + version should be valid, got: %v", err)
	}

	// Name part too long even with version.
	name65 := strings.Repeat("x", 65)
	if err := PackageName(name65 + "@1.0.0"); err == nil {
		t.Error("PackageName with 65-char name + version should be invalid")
	}
}

func TestPackageName_AtOnly(t *testing.T) {
	// "@version" has empty name part.
	if err := PackageName("@1.0"); err == nil {
		t.Error("PackageName(@1.0) should reject empty name part")
	}
}

func TestID_Valid(t *testing.T) {
	valid := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"00000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	}
	for _, id := range valid {
		if err := ID(id); err != nil {
			t.Errorf("ID(%q) should be valid, got: %v", id, err)
		}
	}
}

func TestID_Invalid(t *testing.T) {
	invalid := []string{
		"",                    // empty
		"not-a-uuid",         // too short
		"550e8400-e29b-41d4-a716-44665544000",  // 35 chars
		"550e8400-e29b-41d4-a716-4466554400000", // 37 chars
		"550e8400-e29b-41d4-a716-44665544000G",  // uppercase G not in [a-f0-9-]
		"FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF",  // uppercase hex
	}
	for _, id := range invalid {
		if err := ID(id); err == nil {
			t.Errorf("ID(%q) should be invalid", id)
		}
	}
}

func TestStringLength(t *testing.T) {
	// Within limit.
	if err := StringLength("field", "hello", 10); err != nil {
		t.Errorf("StringLength should accept 5 chars with max 10: %v", err)
	}

	// Exactly at limit.
	if err := StringLength("field", "hello", 5); err != nil {
		t.Errorf("StringLength should accept 5 chars with max 5: %v", err)
	}

	// Over limit.
	if err := StringLength("field", "hello!", 5); err == nil {
		t.Error("StringLength should reject 6 chars with max 5")
	}

	// Empty string is always valid.
	if err := StringLength("field", "", 0); err != nil {
		t.Errorf("StringLength should accept empty string with max 0: %v", err)
	}
}

func TestStringLength_ErrorMessage(t *testing.T) {
	err := StringLength("description", "toolong", 3)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "description") {
		t.Errorf("error should contain field name, got: %s", msg)
	}
	if !strings.Contains(msg, "7") {
		t.Errorf("error should contain actual length, got: %s", msg)
	}
	if !strings.Contains(msg, "3") {
		t.Errorf("error should contain max length, got: %s", msg)
	}
}

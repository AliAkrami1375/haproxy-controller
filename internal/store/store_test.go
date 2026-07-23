package store

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{"web", "web_in", "app-1", "a.b.c", "A1"}
	for _, n := range valid {
		if err := ValidateName("frontend", n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	// Names become section headers, so anything that could break out of the
	// line, or start one, must be refused.
	invalid := []string{"", "-lead", ".lead", "has space", "new\nline", "semi;colon", "hash#", strings.Repeat("a", 64)}
	for _, n := range invalid {
		if err := ValidateName("frontend", n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", n)
		}
	}
}

func TestValidateHostname(t *testing.T) {
	for _, h := range []string{"example.com", "a.b.example.com", "*.example.com", "xn--80ak6aa92e.com"} {
		if err := ValidateHostname(h); err != nil {
			t.Errorf("ValidateHostname(%q) = %v, want nil", h, err)
		}
	}
	for _, h := range []string{"", "has space.com", "under_score.com", "trailing-.com", strings.Repeat("a", 254)} {
		if err := ValidateHostname(h); err == nil {
			t.Errorf("ValidateHostname(%q) = nil, want an error", h)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("Str0ngEnough"); err != nil {
		t.Errorf("ValidatePassword() = %v, want nil", err)
	}
	for _, pw := range []string{"short1", "nodigitshere", "1234567890"} {
		if err := ValidatePassword(pw); err == nil {
			t.Errorf("ValidatePassword(%q) = nil, want an error", pw)
		}
	}
}

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("Str0ngEnough")
	if err != nil {
		t.Fatalf("HashPassword() = %v", err)
	}
	if strings.Contains(hash, "Str0ngEnough") {
		t.Error("the hash must not contain the password")
	}
	if !VerifyPassword(hash, "Str0ngEnough") {
		t.Error("VerifyPassword() rejected the correct password")
	}
	if VerifyPassword(hash, "wrong-password-1") {
		t.Error("VerifyPassword() accepted an incorrect password")
	}
}

func TestGeneratePasswordMeetsPolicy(t *testing.T) {
	for i := 0; i < 25; i++ {
		pw, err := GeneratePassword(20)
		if err != nil {
			t.Fatalf("GeneratePassword() = %v", err)
		}
		if len(pw) != 20 {
			t.Fatalf("GeneratePassword(20) produced %d characters", len(pw))
		}
		if err := ValidatePassword(pw); err != nil {
			t.Fatalf("generated password %q fails the policy: %v", pw, err)
		}
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !ConstantTimeEqual("abc123", "abc123") {
		t.Error("ConstantTimeEqual() rejected identical strings")
	}
	if ConstantTimeEqual("abc123", "abc124") || ConstantTimeEqual("abc", "abcd") {
		t.Error("ConstantTimeEqual() accepted differing strings")
	}
}

// The rendered error page is a raw HTTP response, so Content-Length has to
// match the CRLF-normalised body exactly or HAProxy truncates or hangs.
func TestErrorPageRawHTTP(t *testing.T) {
	p := &ErrorPage{
		StatusCode:  503,
		ContentType: "text/html; charset=utf-8",
		Headers:     "Retry-After: 30",
		Body:        "<h1>Down</h1>\nsecond line\n",
	}
	raw := p.RawHTTP()

	if !strings.HasPrefix(raw, "HTTP/1.1 503 Service Unavailable\r\n") {
		t.Errorf("RawHTTP() status line = %q", strings.SplitN(raw, "\r\n", 2)[0])
	}
	for _, want := range []string{"Content-Type: text/html; charset=utf-8\r\n", "Retry-After: 30\r\n", "Connection: close\r\n"} {
		if !strings.Contains(raw, want) {
			t.Errorf("RawHTTP() is missing %q", want)
		}
	}

	head, body, found := strings.Cut(raw, "\r\n\r\n")
	if !found {
		t.Fatal("RawHTTP() has no header/body separator")
	}
	var declared string
	for _, line := range strings.Split(head, "\r\n") {
		if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			declared = v
		}
	}
	if declared == "" {
		t.Fatal("RawHTTP() declared no Content-Length")
	}
	if declared != itoa(len(body)) {
		t.Errorf("Content-Length = %s but the body is %d bytes", declared, len(body))
	}
	if strings.Contains(body, "\n") && !strings.Contains(body, "\r\n") {
		t.Error("the body must use CRLF line endings")
	}
}

func TestErrorPageFileNameIsUniquePerGroupAndCode(t *testing.T) {
	a := (&ErrorPage{Name: "x", GroupName: "default", StatusCode: 503}).FileName()
	b := (&ErrorPage{Name: "y", GroupName: "maintenance", StatusCode: 503}).FileName()
	if a == b {
		t.Errorf("two groups produced the same file name: %q", a)
	}
	if a != "default-503.http" {
		t.Errorf("FileName() = %q, want default-503.http", a)
	}
}

func TestSplitLines(t *testing.T) {
	got := SplitLines("  a  \n\n b\n\t\n c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("SplitLines() = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SplitLines()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

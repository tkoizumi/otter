package inspection

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixturePath locates the corpus shared with the Python SDK. Both
// implementations read the same file so redaction cannot drift between them.
func fixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "capture", "redaction_fixtures.json")
}

type redactionFixtures struct {
	Version int `json:"version"`
	URLs    []struct {
		Name       string `json:"name"`
		Input      string `json:"input"`
		Expect     string `json:"expect"`
		Redactions int    `json:"redactions"`
	} `json:"urls"`
	Headers []struct {
		Name       string     `json:"name"`
		Input      [][]string `json:"input"`
		Expect     [][]string `json:"expect"`
		Redactions int        `json:"redactions"`
	} `json:"headers"`
	JSON []struct {
		Name       string `json:"name"`
		Input      string `json:"input"`
		Expect     string `json:"expect"`
		Redactions int    `json:"redactions"`
	} `json:"json"`
	InvalidJSON []struct {
		Name  string `json:"name"`
		Input string `json:"input"`
	} `json:"invalid_json"`
	ErrorText []struct {
		Name             string   `json:"name"`
		Input            string   `json:"input"`
		ExpectContains   []string `json:"expect_contains"`
		ExpectNotContain []string `json:"expect_not_contains"`
	} `json:"error_text"`
}

func loadFixtures(t *testing.T) redactionFixtures {
	t.Helper()
	data, err := os.ReadFile(fixturePath(t))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var f redactionFixtures
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if f.Version != 1 {
		t.Fatalf("fixture version = %d, want 1", f.Version)
	}
	if len(f.URLs) == 0 || len(f.Headers) == 0 || len(f.JSON) == 0 {
		t.Fatalf("fixtures are missing a section")
	}
	return f
}

func TestSanitizeURLFixtures(t *testing.T) {
	f := loadFixtures(t)
	r := DefaultRedactor()
	for _, tc := range f.URLs {
		t.Run(tc.Name, func(t *testing.T) {
			got, count := r.SanitizeURL(tc.Input)
			if got != tc.Expect {
				t.Errorf("SanitizeURL(%q) = %q, want %q", tc.Input, got, tc.Expect)
			}
			if count != tc.Redactions {
				t.Errorf("SanitizeURL(%q) redactions = %d, want %d", tc.Input, count, tc.Redactions)
			}
		})
	}
}

func TestSanitizeHeadersFixtures(t *testing.T) {
	f := loadFixtures(t)
	r := DefaultRedactor()
	for _, tc := range f.Headers {
		t.Run(tc.Name, func(t *testing.T) {
			got, count := r.SanitizeHeaders(toPairs(tc.Input))
			if count != tc.Redactions {
				t.Errorf("redactions = %d, want %d", count, tc.Redactions)
			}
			if len(got) != len(tc.Expect) {
				t.Fatalf("got %d pairs, want %d", len(got), len(tc.Expect))
			}
			for i, want := range tc.Expect {
				if got[i].Name != want[0] || got[i].Value != want[1] {
					t.Errorf("pair %d = (%q,%q), want (%q,%q)",
						i, got[i].Name, got[i].Value, want[0], want[1])
				}
			}
		})
	}
}

func TestSanitizeJSONFixtures(t *testing.T) {
	f := loadFixtures(t)
	r := DefaultRedactor()
	for _, tc := range f.JSON {
		t.Run(tc.Name, func(t *testing.T) {
			got, count, err := r.SanitizeJSON([]byte(tc.Input))
			if err != nil {
				t.Fatalf("SanitizeJSON(%q): %v", tc.Input, err)
			}
			if count != tc.Redactions {
				t.Errorf("redactions = %d, want %d (got %s)", count, tc.Redactions, got)
			}
			if !equalJSON(t, got, []byte(tc.Expect)) {
				t.Errorf("SanitizeJSON(%q) = %s, want %s", tc.Input, got, compact(t, []byte(tc.Expect)))
			}
		})
	}
}

func TestSanitizeJSONRejectsInvalidDocuments(t *testing.T) {
	f := loadFixtures(t)
	r := DefaultRedactor()
	for _, tc := range f.InvalidJSON {
		t.Run(tc.Name, func(t *testing.T) {
			got, _, err := r.SanitizeJSON([]byte(tc.Input))
			if err == nil {
				t.Fatalf("SanitizeJSON(%q) = %s, want an error", tc.Input, got)
			}
			if got != nil {
				t.Errorf("a rejected document must not return bytes, got %s", got)
			}
		})
	}
}

func TestSanitizeErrorTextFixtures(t *testing.T) {
	f := loadFixtures(t)
	r := DefaultRedactor()
	for _, tc := range f.ErrorText {
		t.Run(tc.Name, func(t *testing.T) {
			got := r.SanitizeErrorText(tc.Input, DefaultLimits().MaxErrorTextBytes)
			for _, want := range tc.ExpectContains {
				if !strings.Contains(got, want) {
					t.Errorf("SanitizeErrorText(%q) = %q, missing %q", tc.Input, got, want)
				}
			}
			for _, unwanted := range tc.ExpectNotContain {
				if strings.Contains(got, unwanted) {
					t.Errorf("SanitizeErrorText(%q) = %q, still contains %q", tc.Input, got, unwanted)
				}
			}
		})
	}
}

func TestSanitizeErrorTextIsBounded(t *testing.T) {
	r := DefaultRedactor()
	long := strings.Repeat("x", 5000)
	got := r.SanitizeErrorText(long, 64)
	if len(got) > 64+len("…") {
		t.Errorf("bounded text is %d bytes, want at most %d", len(got), 64+len("…"))
	}
}

// TestOperatorRulesAreAdditiveOnly proves an operator can add a rule but cannot
// remove a mandatory one.
func TestOperatorRulesAreAdditiveOnly(t *testing.T) {
	r := NewRedactor([]string{"X-Tenant-Secret"}, nil, []string{"tenant_key"})
	if _, n := r.SanitizeHeaders([]HeaderPair{{Name: "x-tenant-secret", Value: "v"}}); n != 1 {
		t.Errorf("operator header rule not applied")
	}
	if _, n := r.SanitizeHeaders([]HeaderPair{{Name: "Authorization", Value: "v"}}); n != 1 {
		t.Errorf("mandatory authorization rule was weakened")
	}
	got, n, err := r.SanitizeJSON([]byte(`{"tenant_key":"v","password":"p"}`))
	if err != nil {
		t.Fatalf("SanitizeJSON: %v", err)
	}
	if n != 2 || strings.Contains(string(got), "\"p\"") {
		t.Errorf("operator field rule did not compose with the mandatory rule: %s (n=%d)", got, n)
	}
}

func toPairs(rows [][]string) []HeaderPair {
	out := make([]HeaderPair, 0, len(rows))
	for _, row := range rows {
		if len(row) != 2 {
			continue
		}
		out = append(out, HeaderPair{Name: row[0], Value: row[1]})
	}
	return out
}

func compact(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return buf.Bytes()
}

func equalJSON(t *testing.T, got, want []byte) bool {
	t.Helper()
	return bytes.Equal(compact(t, got), compact(t, want))
}

package inspection

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// RedactedPlaceholder replaces every value the policy classifies as secret. It
// is a fixed token rather than an empty string so a reviewer can tell "a secret
// was here" apart from "the field was absent".
const RedactedPlaceholder = "REDACTED"

// sensitiveHeaders are matched after normalization, so case, underscores and
// hyphens do not matter.
var sensitiveHeaders = []string{
	"authorization", "proxyauthorization", "cookie", "setcookie",
	"xapikey", "apikey", "xauthtoken", "xaccesstoken", "xshopifyaccesstoken",
	"xamzsecuritytoken", "xcSRftoken", "csrftoken", "privatetoken",
	"xgitlabtoken", "xhubsignature", "xgoogapikey",
	"ocpapimsubscriptionkey", "subscriptionkey", "xamzcredential", "xamzsignature",
}

// sensitiveQueryNames are matched against the decoded parameter name after the
// same normalization.
var sensitiveQueryNames = []string{
	"accesstoken", "token", "refreshtoken", "idtoken", "apikey", "key",
	"secret", "clientsecret", "password", "passwd", "pwd", "signature", "sig",
	"auth", "authorization", "code", "session", "sessionid", "subscriptionkey",
	"xamzsignature", "xamzcredential", "xamzsecuritytoken",
}

// sensitiveFields are matched against object keys in captured JSON.
var sensitiveFields = []string{
	"password", "passwd", "pwd", "secret", "clientsecret", "token",
	"accesstoken", "refreshtoken", "idtoken", "apikey", "privatekey",
	"authorization", "credentials", "cookie", "setcookie", "session",
	"sessionid", "signature", "nonce",
}

// sensitiveMarkers redact by containment rather than exact match. They are the
// substrings that are almost never innocent in a credential position. The
// trade-off is deliberate: over-redacting a field named "token_count" is
// harmless, while missing one real token is not.
var sensitiveMarkers = []string{
	"password", "passwd", "pwd", "secret", "token", "apikey", "privatekey",
	"credential", "signature", "authorization", "cookie", "session",
}

// Redactor applies the capture redaction policy. A nil *Redactor is not valid;
// use DefaultRedactor or NewRedactor.
type Redactor struct {
	headers map[string]struct{}
	query   map[string]struct{}
	fields  map[string]struct{}
}

// NewRedactor builds a redactor from the mandatory defaults plus any operator
// additions. Operator rules are additive only: there is no way to weaken a
// mandatory rule from here.
func NewRedactor(extraHeaders, extraQuery, extraFields []string) *Redactor {
	r := &Redactor{
		headers: nameSet(sensitiveHeaders),
		query:   nameSet(sensitiveQueryNames),
		fields:  nameSet(sensitiveFields),
	}
	addNames(r.headers, extraHeaders)
	addNames(r.query, extraQuery)
	addNames(r.fields, extraFields)
	return r
}

// DefaultRedactor returns the mandatory policy with no operator additions.
func DefaultRedactor() *Redactor { return NewRedactor(nil, nil, nil) }

// SanitizeURL removes userinfo and the fragment, and redacts the values of
// sensitive query parameters. Non-sensitive parameters, repeated parameters and
// their original encoding are preserved verbatim. It returns the sanitized URL
// and how many secrets were removed.
//
// A URL that cannot be parsed is not stored as-is: everything from the query or
// fragment onward is dropped, so an unparseable URL can never carry a token.
func (r *Redactor) SanitizeURL(raw string) (string, int) {
	if raw == "" {
		return "", 0
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return stripUnparseableURL(raw), 0
	}

	count := 0
	if u.User != nil {
		u.User = nil
		count++
	}
	u.Fragment = ""
	u.RawFragment = ""

	sanitized, redacted := r.sanitizeRawQuery(u.RawQuery)
	u.RawQuery = sanitized
	count += redacted

	return u.String(), count
}

// SanitizeHeaders redacts sensitive header values, preserving order and
// duplicate names.
func (r *Redactor) SanitizeHeaders(pairs []HeaderPair) ([]HeaderPair, int) {
	if len(pairs) == 0 {
		return nil, 0
	}
	out := make([]HeaderPair, 0, len(pairs))
	count := 0
	for _, pair := range pairs {
		clean := HeaderPair{Name: cleanText(pair.Name), Value: cleanText(pair.Value)}
		if r.isSensitiveHeader(clean.Name) {
			clean.Value = RedactedPlaceholder
			count++
		}
		out = append(out, clean)
	}
	return out, count
}

// SanitizeJSON returns a copy of raw with the values of sensitive object fields
// replaced by RedactedPlaceholder. Key order and number literals are preserved;
// insignificant whitespace is normalized. Invalid or trailing-garbage JSON is an
// error, and the caller omits the body rather than storing a partial rewrite.
func (r *Redactor) SanitizeJSON(raw []byte) (json.RawMessage, int, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, 0, fmt.Errorf("empty JSON document")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var buf bytes.Buffer
	count, err := r.writeValue(dec, &buf)
	if err != nil {
		return nil, 0, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, 0, fmt.Errorf("unexpected data after the JSON document")
	}
	return json.RawMessage(buf.Bytes()), count, nil
}

// SanitizeErrorText makes transport-error text safe to persist: control
// characters are removed, embedded userinfo and sensitive query values are
// redacted, and the result is bounded. Exception text is routinely built from a
// URL, so it is never stored verbatim.
func (r *Redactor) SanitizeErrorText(text string, limit int) string {
	text = cleanText(text)
	text = userinfoPattern.ReplaceAllString(text, "${1}"+RedactedPlaceholder+"@")
	text = querySecretPattern.ReplaceAllString(text, "${1}="+RedactedPlaceholder)
	text = strings.TrimSpace(text)
	if limit > 0 && len(text) > limit {
		text = truncateRunes(text, limit) + "…"
	}
	return text
}

// ------------------------------------------------------------------ internals

var (
	// Matches scheme://user:pass@ so embedded credentials can be removed from
	// free-form text.
	userinfoPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]*@`)
	// Matches name=value for a credential-looking name in free-form text.
	querySecretPattern = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:token|secret|password|passwd|pwd|apikey|api_key|signature|sig|session|auth)[a-z0-9_.-]*)=([^&\s'"]+)`)
)

func (r *Redactor) sanitizeRawQuery(rawQuery string) (string, int) {
	if rawQuery == "" {
		return "", 0
	}
	parts := strings.Split(rawQuery, "&")
	count := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		name := part
		if idx := strings.Index(part, "="); idx >= 0 {
			name = part[:idx]
		}
		decoded, err := url.QueryUnescape(name)
		if err != nil {
			decoded = name
		}
		if r.isSensitiveQuery(decoded) {
			parts[i] = name + "=" + RedactedPlaceholder
			count++
		}
	}
	return strings.Join(parts, "&"), count
}

func (r *Redactor) isSensitiveHeader(name string) bool {
	n := normalizeName(name)
	if _, ok := r.headers[n]; ok {
		return true
	}
	return containsMarker(n)
}

func (r *Redactor) isSensitiveQuery(name string) bool {
	n := normalizeName(name)
	if _, ok := r.query[n]; ok {
		return true
	}
	return containsMarker(n)
}

func (r *Redactor) isSensitiveField(name string) bool {
	n := normalizeName(name)
	if _, ok := r.fields[n]; ok {
		return true
	}
	return containsMarker(n)
}

func containsMarker(normalized string) bool {
	for _, marker := range sensitiveMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// normalizeName lowercases a name and drops the separators that distinguish
// camelCase, snake_case and kebab-case spellings of the same credential field.
func normalizeName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch r {
		case '_', '-', ' ', '\t':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func nameSet(names []string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	addNames(out, names)
	return out
}

func addNames(set map[string]struct{}, names []string) {
	for _, name := range names {
		if n := normalizeName(name); n != "" {
			set[n] = struct{}{}
		}
	}
}

// cleanText removes control characters, including CR and LF. Header values and
// error text are attacker-influenced and must not be able to inject lines into
// a terminal or a log.
func cleanText(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\t' {
			b.WriteRune(' ')
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// stripUnparseableURL keeps only the leading portion of a URL that could not be
// parsed, discarding anything that might carry a credential.
func stripUnparseableURL(raw string) string {
	out := raw
	if idx := strings.IndexAny(out, "?#"); idx >= 0 {
		out = out[:idx]
	}
	out = userinfoPattern.ReplaceAllString(out, "${1}"+RedactedPlaceholder+"@")
	return cleanText(out)
}

func truncateRunes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func writeJSONString(buf *bytes.Buffer, s string) error {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.WriteString(strings.TrimSuffix(sb.String(), "\n"))
	return nil
}

func (r *Redactor) writeValue(dec *json.Decoder, buf *bytes.Buffer) (int, error) {
	tok, err := dec.Token()
	if err != nil {
		return 0, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return 0, writeScalar(buf, tok)
	}
	switch delim {
	case '{':
		return r.writeObject(dec, buf)
	case '[':
		return r.writeArray(dec, buf)
	default:
		return 0, fmt.Errorf("unexpected %q in JSON document", delim)
	}
}

func (r *Redactor) writeObject(dec *json.Decoder, buf *bytes.Buffer) (int, error) {
	buf.WriteByte('{')
	count := 0
	first := true
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return count, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return count, fmt.Errorf("object key is not a string")
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		if err := writeJSONString(buf, key); err != nil {
			return count, err
		}
		buf.WriteByte(':')

		if r.isSensitiveField(key) {
			if _, err := skipValue(dec); err != nil {
				return count, err
			}
			if err := writeJSONString(buf, RedactedPlaceholder); err != nil {
				return count, err
			}
			count++
			continue
		}
		c, err := r.writeValue(dec, buf)
		if err != nil {
			return count, err
		}
		count += c
	}
	if _, err := dec.Token(); err != nil {
		return count, err
	}
	buf.WriteByte('}')
	return count, nil
}

func (r *Redactor) writeArray(dec *json.Decoder, buf *bytes.Buffer) (int, error) {
	buf.WriteByte('[')
	count := 0
	first := true
	for dec.More() {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		c, err := r.writeValue(dec, buf)
		if err != nil {
			return count, err
		}
		count += c
	}
	if _, err := dec.Token(); err != nil {
		return count, err
	}
	buf.WriteByte(']')
	return count, nil
}

func writeScalar(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case json.Number:
		buf.WriteString(t.String())
		return nil
	case string:
		return writeJSONString(buf, t)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(encoded)
		return nil
	}
}

// skipValue consumes one complete JSON value without emitting it.
func skipValue(dec *json.Decoder) (int, error) {
	tok, err := dec.Token()
	if err != nil {
		return 0, err
	}
	if _, ok := tok.(json.Delim); !ok {
		return 1, nil
	}
	depth := 1
	for depth > 0 {
		t, err := dec.Token()
		if err != nil {
			return 0, err
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return 1, nil
}

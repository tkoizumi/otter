// Package awsv4 signs AWS API requests with Signature Version 4.
//
// Implemented rather than imported because an SDK would bring a dependency
// chain into binaries that run on every pool host. It has two callers with
// different needs — the agent's bootstrap assertion, and the host broker's read
// of AWS Secrets Manager — which is why it lives here rather than inside either
// one. The canonicalisation rules are the part that must be exactly right, so
// they are separated and tested.
package awsv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Input is everything the signature depends on. It is a struct rather than a
// parameter list so a caller cannot silently omit a field that changes the
// signature's meaning.
type Input struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
	Service         string
	Method          string
	Host            string
	Path            string
	// Query is the canonical query string, already sorted and encoded. Empty
	// means no query, which is the case for every caller today.
	Query   string
	Payload []byte
	Now     time.Time

	// Headers are extra headers to sign, keyed by any case. The signer always
	// signs host, x-amz-date and x-amz-content-sha256, plus
	// x-amz-security-token when a session token is present.
	//
	// A caller adding a header here is promising the request will actually send
	// it: a signed header that is absent makes the signature unreproducible, and
	// a verifier reports it as a missing header rather than as a mismatch.
	Headers map[string]string
}

// Sign produces the Authorization header value for a request.
func Sign(in Input) (string, error) {
	if in.AccessKeyID == "" || in.SecretAccessKey == "" {
		return "", fmt.Errorf("awsv4: sigv4 needs credentials")
	}
	if in.Region == "" || in.Service == "" {
		return "", fmt.Errorf("awsv4: sigv4 needs a region and service")
	}
	if in.Now.IsZero() {
		return "", fmt.Errorf("awsv4: sigv4 needs a signing time")
	}

	amzDate := in.Now.UTC().Format("20060102T150405Z")
	dateStamp := in.Now.UTC().Format("20060102")

	payloadHash := Sha256Hex(in.Payload)

	headers := map[string]string{
		"host":                 in.Host,
		"x-amz-date":           amzDate,
		"x-amz-content-sha256": payloadHash,
	}
	if in.SessionToken != "" {
		headers["x-amz-security-token"] = in.SessionToken
	}
	for name, value := range in.Headers {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "" {
			continue
		}
		headers[lower] = value
	}

	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, k := range names {
		canonicalHeaders.WriteString(k)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(strings.TrimSpace(headers[k]))
		canonicalHeaders.WriteString("\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		in.Method,
		CanonicalURI(in.Path),
		in.Query,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, in.Region, in.Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		Sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+in.SecretAccessKey), dateStamp)
	kRegion := hmacSHA256(kDate, in.Region)
	kService := hmacSHA256(kRegion, in.Service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	return fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		in.AccessKeyID, scope, signedHeaders, signature), nil
}

// CanonicalURI applies the SigV4 rule: each path segment is URI-encoded, and "/"
// separators are preserved.
func CanonicalURI(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	segments := strings.Split(p, "/")
	for i, segment := range segments {
		segments[i] = awsURIEncode(segment, false)
	}
	return strings.Join(segments, "/")
}

// awsURIEncode encodes per SigV4: unreserved characters pass through, everything
// else is percent-encoded uppercase-hex, and a space becomes %20 rather than +.
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}

// Sha256Hex is the lowercase hex SHA-256 of b, which is what SigV4 signs in
// place of a payload.
func Sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// NormaliseHeaderNames lowercases header names, because a signature covers
// lowercase names and a verifier reading "X-Amz-Date" would not find the
// "x-amz-date" it signed.
func NormaliseHeaderNames(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}

// Header renders the signed values as request headers, so a caller and any test
// client send exactly what was signed.
func (in Input) Header() (http.Header, error) {
	auth, err := Sign(in)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	// Host is signed, so it must be returned: a verifier re-derives the signature
	// from the headers it RECEIVES, and omitting a header the signature covers
	// makes it unreproducible -- reported as "signed header host is missing", which
	// names the symptom and not the cause.
	h.Set("Host", in.Host)
	h.Set("Authorization", auth)
	h.Set("X-Amz-Date", in.Now.UTC().Format("20060102T150405Z"))
	h.Set("X-Amz-Content-Sha256", Sha256Hex(in.Payload))
	if in.SessionToken != "" {
		h.Set("X-Amz-Security-Token", in.SessionToken)
	}
	for name, value := range in.Headers {
		h.Set(name, value)
	}
	return h, nil
}

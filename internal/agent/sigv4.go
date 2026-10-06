package agent

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

// sigV4Input is everything the signature depends on. It is a struct rather than
// a parameter list so a caller cannot silently omit a field that changes the
// signature's meaning.
type sigV4Input struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
	Service         string
	Method          string
	Host            string
	Path            string
	Payload         []byte
	Now             time.Time
}

// signSigV4 produces the Authorization header value for a request.
//
// Implemented rather than imported because the agent is the only component that
// needs it and an SDK would bring a dependency chain into a binary that runs on
// every pool host. The canonicalisation rules are the part that must be exactly
// right, so they are separated and tested.
func signSigV4(in sigV4Input) (string, error) {
	if in.AccessKeyID == "" || in.SecretAccessKey == "" {
		return "", fmt.Errorf("agent: sigv4 needs credentials")
	}
	if in.Region == "" || in.Service == "" {
		return "", fmt.Errorf("agent: sigv4 needs a region and service")
	}
	if in.Now.IsZero() {
		return "", fmt.Errorf("agent: sigv4 needs a signing time")
	}

	amzDate := in.Now.UTC().Format("20060102T150405Z")
	dateStamp := in.Now.UTC().Format("20060102")

	payloadHash := sha256Hex(in.Payload)

	// Canonical headers must be lowercase, sorted, and trimmed. Content-type is
	// included because the control plane will sign the same set; a mismatch here
	// is the most common cause of a signature that cannot be reproduced.
	headers := map[string]string{
		"host":                 in.Host,
		"x-amz-date":           amzDate,
		"x-amz-content-sha256": payloadHash,
	}
	if in.SessionToken != "" {
		headers["x-amz-security-token"] = in.SessionToken
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
		canonicalURI(in.Path),
		"", // query string: the bootstrap request carries none
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, in.Region, in.Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
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

// canonicalURI applies the SigV4 rule: each path segment is URI-encoded, and "/"
// separators are preserved.
func canonicalURI(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsURIEncode(s, false)
	}
	return strings.Join(segs, "/")
}

// awsURIEncode encodes per SigV4: unreserved characters pass through, everything
// else is percent-encoded uppercase-hex, and a space becomes %20 rather than +.
func awsURIEncode(s string, encodeSlash bool) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		if c == '/' && !encodeSlash {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// Header renders the signed values as request headers, so the agent and any test
// client send exactly what was signed.
func (in sigV4Input) Header() (http.Header, error) {
	auth, err := signSigV4(in)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Authorization", auth)
	h.Set("X-Amz-Date", in.Now.UTC().Format("20060102T150405Z"))
	h.Set("X-Amz-Content-Sha256", sha256Hex(in.Payload))
	if in.SessionToken != "" {
		h.Set("X-Amz-Security-Token", in.SessionToken)
	}
	return h, nil
}

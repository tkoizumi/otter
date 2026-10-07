package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// digest is a well-formed bare release digest; the client must send exactly it
// in the digest header and in desired_digest.
const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestMeVerifiesTheCredential(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"organization":{"id":"org_1","name":"Acme"},`+
			`"runtimes":[{"id":"rt_1","lifecycle":"running","placement":"aws"}]}`)
	}))
	defer srv.Close()

	me, err := NewClient(srv.URL, "otk_1_secret").Me(context.Background())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if gotPath != "/api/cloud/me" {
		t.Errorf("path = %q, want /api/cloud/me", gotPath)
	}
	if gotAuth != "Bearer otk_1_secret" {
		t.Errorf("Authorization = %q, want the bearer token", gotAuth)
	}
	if me.Organization.ID != "org_1" || me.Organization.Name != "Acme" {
		t.Errorf("organization = %+v", me.Organization)
	}
	if len(me.Runtimes) != 1 || me.Runtimes[0].ID != "rt_1" || me.Runtimes[0].Lifecycle != "running" {
		t.Errorf("runtimes = %+v", me.Runtimes)
	}
}

func TestMeReportsARevokedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"token revoked"}`)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "otk_1_secret").Me(context.Background())
	if err == nil {
		t.Fatal("a 401 was accepted")
	}
	if !IsUnauthorized(err) {
		t.Errorf("IsUnauthorized(%v) = false", err)
	}
	if !strings.Contains(err.Error(), "token revoked") {
		t.Errorf("error does not carry the server message: %v", err)
	}
}

func TestUploadReleaseSendsTheBareDigestHeaderAndTheArchive(t *testing.T) {
	archive := []byte("this is a package tarball, honest")
	var gotAuth, gotDigest, gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDigest = r.Header.Get("x-otter-release-digest")
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"rel_1","digest":"`+digest+`"}`)
	}))
	defer srv.Close()

	rel, err := NewClient(srv.URL, "otk_1_secret").
		UploadRelease(context.Background(), "sha256:"+digest, bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("UploadRelease: %v", err)
	}
	if gotAuth != "Bearer otk_1_secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotDigest != digest {
		t.Errorf("x-otter-release-digest = %q, want the bare digest %q", gotDigest, digest)
	}
	if gotType != "application/octet-stream" {
		t.Errorf("Content-Type = %q", gotType)
	}
	if !bytes.Equal(gotBody, archive) {
		t.Errorf("body = %q, want the archive bytes", gotBody)
	}
	if rel.ID != "rel_1" {
		t.Errorf("release = %+v", rel)
	}
}

func TestDeploySendsTheDigestAndExpectedGeneration(t *testing.T) {
	var gotAuth, gotPath string
	var body DeployRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		io.WriteString(w, `{"admitted":true,"operation":{"id":"op_1","status":"queued"}}`)
	}))
	defer srv.Close()

	admission, err := NewClient(srv.URL, "otk_1_secret").
		Deploy(context.Background(), "rt_1", "sha256:"+digest, "ops@acme.example", 7)
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if gotPath != "/api/runtimes/rt_1/operations" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer otk_1_secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if body.Action != "deploy" || body.DesiredDigest != digest || body.ExpectedGeneration != 7 {
		t.Errorf("request body = %+v", body)
	}
	if body.Operator != "ops@acme.example" {
		t.Errorf("operator = %q", body.Operator)
	}
	if !admission.Admitted || admission.Operation == nil || admission.Operation.ID != "op_1" {
		t.Errorf("admission = %+v", admission)
	}
}

func TestDeployRefusalCarriesTheServerMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"admitted":false,"message":"generation moved; re-read and retry"}`)
	}))
	defer srv.Close()

	admission, err := NewClient(srv.URL, "otk_1_secret").
		Deploy(context.Background(), "rt_1", digest, "ops", 3)
	if err == nil {
		t.Fatal("an admitted=false answer was treated as success")
	}
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error is %T, want *RefusedError", err)
	}
	if refused.Message != "generation moved; re-read and retry" {
		t.Errorf("message = %q", refused.Message)
	}
	if admission == nil || admission.Admitted {
		t.Errorf("admission = %+v", admission)
	}
}

func TestOperationsReadsTheGeneration(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{"generation":42,"operations":[{"id":"op_0","status":"succeeded"}]}`)
	}))
	defer srv.Close()

	state, err := NewClient(srv.URL, "otk_1_secret").Operations(context.Background(), "rt_1")
	if err != nil {
		t.Fatalf("Operations: %v", err)
	}
	if gotPath != "/api/runtimes/rt_1/operations" {
		t.Errorf("path = %q", gotPath)
	}
	if state.Generation != 42 {
		t.Errorf("generation = %d, want 42", state.Generation)
	}
	if len(state.Operations) != 1 || state.Operations[0].ID != "op_0" {
		t.Errorf("operations = %+v", state.Operations)
	}
}

func TestErrorBodyFallsBackToRawText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, "generation conflict")
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "tok").Operations(context.Background(), "rt_1")
	if err == nil || !strings.Contains(err.Error(), "generation conflict") {
		t.Fatalf("err = %v, want the raw body text", err)
	}
}

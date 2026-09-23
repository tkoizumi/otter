package config

import (
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/otter/internal/inspection"
)

func TestDefaultDaemonConfig(t *testing.T) {
	c := DefaultDaemonConfig("1.2.3")

	if c.IntegrationsDir != DefaultIntegrations {
		t.Errorf("IntegrationsDir = %q, want %q", c.IntegrationsDir, DefaultIntegrations)
	}
	if c.DataDir != DefaultDataDir {
		t.Errorf("DataDir = %q, want %q", c.DataDir, DefaultDataDir)
	}
	if c.Listen != DefaultListen {
		t.Errorf("Listen = %q, want %q", c.Listen, DefaultListen)
	}
	if c.Version != "1.2.3" {
		t.Errorf("Version = %q", c.Version)
	}
	if c.Workers < 1 {
		t.Errorf("Workers = %d, want at least 1", c.Workers)
	}
	if !c.ListenIsLoopback() {
		t.Errorf("the default listen address %q should be loopback", c.Listen)
	}
	// Notification is opt-in: the default must not reach out anywhere.
	if c.Notify.Enabled() {
		t.Error("the default configuration enables notification")
	}
}

func TestDefaultWorkersIsCapped(t *testing.T) {
	if got := DefaultWorkers(); got < 1 || got > maxDefaultWorkers {
		t.Errorf("DefaultWorkers = %d, want between 1 and %d", got, maxDefaultWorkers)
	}
}

func TestApplyEnv(t *testing.T) {
	t.Setenv("OTTER_INTEGRATIONS_DIR", "/srv/integrations")
	t.Setenv("OTTER_DATA_DIR", "/var/lib/otter")
	t.Setenv("OTTER_LISTEN", "127.0.0.1:9999")
	t.Setenv("OTTER_WORKERS", "3")
	t.Setenv("OTTER_LOG_FORMAT", "pretty")
	t.Setenv("OTTER_LOG_LEVEL", "debug")
	t.Setenv("OTTER_SHUTDOWN_GRACE", "45s")
	t.Setenv("OTTER_API_TOKEN", "tok")

	c := DefaultDaemonConfig("test")
	if err := c.ApplyEnv(); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}

	if c.IntegrationsDir != "/srv/integrations" || c.DataDir != "/var/lib/otter" {
		t.Errorf("directories not applied: %+v", c)
	}
	if c.Listen != "127.0.0.1:9999" || c.Workers != 3 {
		t.Errorf("listen/workers not applied: %+v", c)
	}
	if c.LogFormat != "pretty" || c.LogLevel != "debug" {
		t.Errorf("logging not applied: %+v", c)
	}
	if c.ShutdownGrace != 45*time.Second {
		t.Errorf("ShutdownGrace = %s, want 45s", c.ShutdownGrace)
	}
	if c.APIToken != "tok" {
		t.Errorf("APIToken = %q", c.APIToken)
	}
}

// Malformed environment values must be reported rather than ignored: silently
// keeping a default is how a typo becomes a mystery at run time.
func TestApplyEnvRejectsMalformedValues(t *testing.T) {
	t.Setenv("OTTER_WORKERS", "lots")
	c := DefaultDaemonConfig("test")
	if err := c.ApplyEnv(); err == nil {
		t.Error("a non-numeric OTTER_WORKERS was accepted")
	}

	t.Setenv("OTTER_WORKERS", "")
	t.Setenv("OTTER_SHUTDOWN_GRACE", "soon")
	c = DefaultDaemonConfig("test")
	if err := c.ApplyEnv(); err == nil {
		t.Error("a non-duration OTTER_SHUTDOWN_GRACE was accepted")
	}
}

func TestApplyEnvNotify(t *testing.T) {
	t.Setenv("OTTER_NOTIFY_URL", "https://example.test/hook")
	t.Setenv("OTTER_NOTIFY_ON", "failed, timed_out")

	c := DefaultDaemonConfig("test")
	if err := c.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if !c.Notify.Enabled() {
		t.Fatal("OTTER_NOTIFY_URL did not enable notification")
	}
	if len(c.Notify.On) != 2 || c.Notify.On[0] != "failed" || c.Notify.On[1] != "timed_out" {
		t.Errorf("On = %v, want [failed timed_out]", c.Notify.On)
	}
}

// Flags win over the environment, which is the documented precedence.
func TestFlagsOverrideEnv(t *testing.T) {
	t.Setenv("OTTER_LISTEN", "127.0.0.1:1111")

	c := DefaultDaemonConfig("test")
	if err := c.ApplyEnv(); err != nil {
		t.Fatal(err)
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	c.RegisterFlags(fs)
	if err := fs.Parse([]string{"--listen", "127.0.0.1:2222"}); err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:2222" {
		t.Errorf("Listen = %q, want the flag value", c.Listen)
	}
}

func TestValidate(t *testing.T) {
	valid := func() DaemonConfig {
		c := DefaultDaemonConfig("test")
		c.Listen = "127.0.0.1:7337"
		return c
	}

	tests := []struct {
		name   string
		mutate func(*DaemonConfig)
	}{
		{"empty integrations dir", func(c *DaemonConfig) { c.IntegrationsDir = "" }},
		{"empty data dir", func(c *DaemonConfig) { c.DataDir = "" }},
		{"listen without port", func(c *DaemonConfig) { c.Listen = "127.0.0.1" }},
		{"zero workers", func(c *DaemonConfig) { c.Workers = 0 }},
		{"negative grace", func(c *DaemonConfig) { c.ShutdownGrace = -time.Second }},
		{"unknown log format", func(c *DaemonConfig) { c.LogFormat = "xml" }},
		{"unknown log level", func(c *DaemonConfig) { c.LogLevel = "loud" }},
	}
	for _, tc := range tests {
		c := valid()
		tc.mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted the configuration", tc.name)
		}
	}

	ok := valid()
	if err := ok.Validate(); err != nil {
		t.Errorf("a default configuration should be valid: %v", err)
	}
}

// Binding a reachable API without a token would expose arbitrary execution to
// the network, so it is refused rather than warned about.
func TestValidateRefusesPublicListenWithoutToken(t *testing.T) {
	c := DefaultDaemonConfig("test")
	c.Listen = "0.0.0.0:7337"

	if err := c.Validate(); err == nil {
		t.Fatal("a wildcard listen without a token was accepted")
	}

	c.APIToken = "tok"
	if err := c.Validate(); err != nil {
		t.Errorf("a wildcard listen with a token should be allowed: %v", err)
	}
}

func TestListenIsLoopback(t *testing.T) {
	tests := []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:7337", true},
		{"localhost:7337", true},
		{"[::1]:7337", true},
		{"0.0.0.0:7337", false},
		{":7337", false},
		{"10.0.0.5:7337", false},
		{"not-an-address", false},
	}
	for _, tc := range tests {
		c := DaemonConfig{Listen: tc.listen}
		if got := c.ListenIsLoopback(); got != tc.want {
			t.Errorf("ListenIsLoopback(%q) = %v, want %v", tc.listen, got, tc.want)
		}
	}
}

// Children always receive a concrete loopback address, never a wildcard bind,
// or they would be handed a URL they cannot dial.
func TestChildAPIURL(t *testing.T) {
	tests := []struct {
		listen string
		want   string
	}{
		{"127.0.0.1:7337", "http://127.0.0.1:7337"},
		{"0.0.0.0:7337", "http://127.0.0.1:7337"},
		{":7337", "http://127.0.0.1:7337"},
		{"[::]:7337", "http://127.0.0.1:7337"},
	}
	for _, tc := range tests {
		c := DaemonConfig{Listen: tc.listen}
		if got := c.ChildAPIURL(); got != tc.want {
			t.Errorf("ChildAPIURL(%q) = %q, want %q", tc.listen, got, tc.want)
		}
	}
}

func TestNotifyConfigMatches(t *testing.T) {
	disabled := NotifyConfig{}
	if disabled.Enabled() || disabled.Matches("failed") {
		t.Error("an unset URL should disable notification entirely")
	}

	all := NotifyConfig{URL: "https://example.test/hook"}
	for _, status := range []string{"failed", "timed_out", "cancelled"} {
		if !all.Matches(status) {
			t.Errorf("default notify config did not match %q", status)
		}
	}
	if all.Matches("succeeded") {
		t.Error("success must never be reported")
	}

	only := NotifyConfig{URL: "https://example.test/hook", On: []string{"failed"}}
	if !only.Matches("failed") || only.Matches("timed_out") {
		t.Error("the allow-list was not respected")
	}
}

// The URL can carry a secret, so the value reported to logs and diagnostics
// must not contain it.
func TestNotifyConfigRedactsURL(t *testing.T) {
	c := NotifyConfig{URL: "https://hooks.slack.com/services/SECRET"}
	if got := c.RedactedURL(); got != "https://hooks.slack.com/..." {
		t.Errorf("RedactedURL = %q, want the host only", got)
	}
	if (NotifyConfig{}).RedactedURL() != "" {
		t.Error("an unset URL should redact to empty")
	}
}

func TestNotifyValidation(t *testing.T) {
	base := func() DaemonConfig {
		c := DefaultDaemonConfig("test")
		c.Listen = "127.0.0.1:7337"
		return c
	}

	bad := base()
	bad.Notify.URL = "not a url"
	if err := bad.Validate(); err == nil {
		t.Error("a malformed notify URL was accepted")
	}

	// The error must not echo a URL that may contain a secret.
	secret := base()
	secret.Notify.URL = "http://example.test/secret-token"
	secret.Notify.On = []string{"exploded"}
	err := secret.Validate()
	if err == nil {
		t.Fatal("an unknown notify status was accepted")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("the validation error leaked the URL: %v", err)
	}

	none := base()
	if err := none.Validate(); err != nil {
		t.Errorf("no notify configuration should validate: %v", err)
	}
}

func TestSplitList(t *testing.T) {
	got := SplitList("failed, timed_out ,, cancelled")
	want := []string{"failed", "timed_out", "cancelled"}
	if len(got) != len(want) {
		t.Fatalf("SplitList = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SplitList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if len(SplitList("  ")) != 0 {
		t.Error("SplitList should drop empty entries")
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func TestNotifyFormat(t *testing.T) {
	// The default is the full payload, and it is spelled out in the config.
	if got := (NotifyConfig{}).FormatOrJSON(); got != FormatJSON {
		t.Errorf("default format = %q, want %q", got, FormatJSON)
	}
	if (NotifyConfig{}).IsChatFormat() {
		t.Error("the default format should not be treated as a chat shape")
	}

	for _, format := range NotifyFormats() {
		c := NotifyConfig{URL: "https://example.test/hook", Format: format}
		if err := c.ValidateFormat(); err != nil {
			t.Errorf("format %q was rejected: %v", format, err)
		}
	}

	// Unknown formats are refused rather than silently sending a body the
	// endpoint cannot read.
	c := NotifyConfig{URL: "https://example.test/hook", Format: "carrier-pigeon"}
	if err := c.ValidateFormat(); err == nil {
		t.Error("an unknown format was accepted")
	}

	// Chat formats are the ones whose shape is dictated by a service.
	if !(NotifyConfig{Format: FormatSlack}).IsChatFormat() {
		t.Error("slack should be a chat format")
	}
	if (NotifyConfig{Format: FormatJSON}).IsChatFormat() {
		t.Error("json should not be a chat format")
	}
}

func TestCaptureDefaultPolicy(t *testing.T) {
	// The shipped default records payloads, so an unattended scheduled run is
	// diagnosable after it has already failed.
	if got := DefaultDaemonConfig("test").CaptureDefaultPolicy(); got != inspection.PolicyFull {
		t.Errorf("default capture policy = %q, want full", got)
	}

	// A configuration built in code that leaves the field unset still resolves
	// to the built-in default rather than capturing nothing.
	var zero DaemonConfig
	if got := zero.CaptureDefaultPolicy(); got != DefaultCapturePolicy {
		t.Errorf("unset capture policy = %q, want %q", got, DefaultCapturePolicy)
	}

	t.Setenv("OTTER_CAPTURE_DEFAULT", "metadata")
	c := DefaultDaemonConfig("test")
	if err := c.ApplyEnv(); err != nil {
		t.Fatalf("ApplyEnv() error = %v", err)
	}
	if got := c.CaptureDefaultPolicy(); got != inspection.PolicyMetadata {
		t.Errorf("capture policy from env = %q, want metadata", got)
	}

	// A typo must stop the daemon rather than leave capture at a value nobody
	// chose.
	bad := DefaultDaemonConfig("test")
	bad.CaptureDefault = "everything"
	if err := bad.Validate(); err == nil {
		t.Error("an invalid --capture-default was accepted")
	}
}

func TestCaptureRedactListsFromEnv(t *testing.T) {
	t.Setenv("OTTER_CAPTURE_REDACT_HEADERS", "X-Tenant-Key, X-Trace-Id")
	t.Setenv("OTTER_CAPTURE_REDACT_QUERY", "session")
	t.Setenv("OTTER_CAPTURE_REDACT_FIELDS", "patient_id, ssn")

	c := DefaultDaemonConfig("test")
	if err := c.ApplyEnv(); err != nil {
		t.Fatalf("ApplyEnv() error = %v", err)
	}

	want := func(name string, got, expected []string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(expected, ",") {
			t.Errorf("%s = %v, want %v", name, got, expected)
		}
	}
	want("CaptureRedactHeaders", c.CaptureRedactHeaders, []string{"X-Tenant-Key", "X-Trace-Id"})
	want("CaptureRedactQuery", c.CaptureRedactQuery, []string{"session"})
	want("CaptureRedactFields", c.CaptureRedactFields, []string{"patient_id", "ssn"})
}

// The default capture policy must survive the flag parser, which is the path
// `otter serve` and `otterd` use.
func TestCaptureDefaultFlag(t *testing.T) {
	c := DefaultDaemonConfig("test")
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	c.RegisterFlags(fs)
	if err := fs.Parse([]string{"--capture-default", "off"}); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got := c.CaptureDefaultPolicy(); got != inspection.PolicyOff {
		t.Errorf("capture policy after flag = %q, want off", got)
	}

	// A typo is refused by the parser, so the daemon never starts on a policy
	// nobody chose.
	bad := DefaultDaemonConfig("test")
	badFlags := flag.NewFlagSet("serve", flag.ContinueOnError)
	badFlags.SetOutput(io.Discard)
	bad.RegisterFlags(badFlags)
	if err := badFlags.Parse([]string{"--capture-default", "everything"}); err == nil {
		t.Error("an invalid --capture-default was accepted by the flag parser")
	}
}

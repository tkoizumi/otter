package config

import (
	"fmt"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a manifest duration. It accepts either a bare number, which is
// interpreted as seconds, or a Go duration string such as "500ms", "30s" or
// "5m". This lets manifests read naturally in both styles:
//
//	timeout: 300
//	timeout: 5m
type Duration time.Duration

// Duration returns the value as a standard library duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the duration in Go's duration syntax.
func (d Duration) String() string { return time.Duration(d).String() }

// Seconds returns the duration rounded down to whole seconds.
func (d Duration) Seconds() int { return int(time.Duration(d) / time.Second) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a number of seconds or a duration string, got %s", kindName(value.Kind))
	}

	raw := value.Value
	if raw == "" || raw == "~" || raw == "null" {
		*d = 0
		return nil
	}

	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		*d = Duration(time.Duration(n) * time.Second)
		return nil
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		*d = Duration(time.Duration(f * float64(time.Second)))
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: expected a number of seconds (300) or a duration string (5m)", raw)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML implements yaml.Marshaler so manifests can be re-emitted.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "a document"
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	default:
		return "an unknown node"
	}
}

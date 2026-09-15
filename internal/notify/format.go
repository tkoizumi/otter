package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/otter-runtime/otter/internal/config"
)

// This file renders the request body for each supported format.
//
// The shapes are dictated by the receiving service, so they live here rather
// than in an integration: a named format can be tested against the contract the
// service publishes, while a template supplied by an operator could not be
// checked at all, and an alerting path that fails silently fails exactly when
// it is needed.
//
// Otter's own JSON remains the default. It carries every field and is what an
// endpoint you wrote, a healthcheck service, or a bridge should receive.

// body renders a payload in the configured format.
func (n *Notifier) body(payload Payload) ([]byte, error) {
	switch n.cfg.FormatOrJSON() {
	case config.FormatJSON:
		return json.Marshal(payload)
	case config.FormatSlack:
		return json.Marshal(map[string]any{"text": chatMessage(payload)})
	case config.FormatDiscord:
		return json.Marshal(map[string]any{"content": chatMessage(payload)})
	case config.FormatTeams:
		return json.Marshal(teamsCard(payload))
	default:
		// Validation rejects an unknown format at startup, so reaching here
		// means the configuration was built in code. Fail loudly rather than
		// silently sending something the endpoint will not understand.
		return nil, fmt.Errorf("notify: unknown format %q", n.cfg.Format)
	}
}

// chatMessage is the one-line human summary used by Slack and Discord.
//
// It deliberately leads with the integration and the integration's own last
// log line, because that is the difference between "exit code 1" and "12
// customers failed to write" -- the thing an operator can act on.
func chatMessage(p Payload) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s *%s* %s", statusEmoji(p.Status), p.Integration, p.Status)
	if p.Attempt > 1 {
		fmt.Fprintf(&b, " (attempt %d)", p.Attempt)
	}

	if p.Detail != "" {
		fmt.Fprintf(&b, "\n%s", p.Detail)
	}
	if p.Error != "" {
		fmt.Fprintf(&b, "\n> %s", p.Error)
	}

	context := []string{"run " + shortID(p.RunID)}
	if p.DurationMS > 0 {
		context = append(context, (time.Duration(p.DurationMS) * time.Millisecond).Round(time.Millisecond).String())
	}
	if p.Release != "" {
		context = append(context, "release "+shortID(p.Release))
	}
	fmt.Fprintf(&b, "\n_%s_", strings.Join(context, " · "))

	return b.String()
}

// statusEmoji gives the message a glance-readable severity.
func statusEmoji(status string) string {
	switch status {
	case "failed":
		return ":red_circle:"
	case "timed_out":
		return ":hourglass:"
	case "cancelled":
		return ":no_entry_sign:"
	default:
		return ":warning:"
	}
}

// shortID trims an identifier to the prefix used elsewhere in Otter's output,
// so a run id in Slack matches the one in `otter runs`.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	if id == "" {
		return "-"
	}
	return id
}

// teamsCard renders a MessageCard, the shape an incoming webhook accepts.
//
// Microsoft has been moving new webhooks to Power Automate Workflows, whose
// body is an Adaptive Card wrapper instead. The MessageCard remains what an
// "Incoming Webhook" connector URL expects, so that is what this emits; a
// Workflows URL needs a bridge, which is documented rather than guessed at.
func teamsCard(p Payload) map[string]any {
	facts := []map[string]string{
		{"name": "Integration", "value": p.Integration},
		{"name": "Status", "value": p.Status},
	}
	if p.Attempt > 1 {
		facts = append(facts, map[string]string{"name": "Attempt", "value": fmt.Sprintf("%d", p.Attempt)})
	}
	if p.DurationMS > 0 {
		facts = append(facts, map[string]string{
			"name":  "Duration",
			"value": (time.Duration(p.DurationMS) * time.Millisecond).Round(time.Millisecond).String(),
		})
	}
	if p.ExitCode != nil {
		facts = append(facts, map[string]string{"name": "Exit code", "value": fmt.Sprintf("%d", *p.ExitCode)})
	}
	facts = append(facts, map[string]string{"name": "Run", "value": p.RunID})
	if p.Release != "" {
		facts = append(facts, map[string]string{"name": "Release", "value": shortID(p.Release)})
	}

	// Detail and error go in the text so they are visible without expanding
	// the card's facts.
	text := p.Detail
	if text == "" {
		text = p.Error
	}

	return map[string]any{
		"@type":    "MessageCard",
		"@context": "https://schema.org/extensions",
		"themeColor": map[string]string{
			"failed":    "D93F0B",
			"timed_out": "E36209",
		}[p.Status],
		"summary": fmt.Sprintf("%s %s", p.Integration, p.Status),
		"title":   fmt.Sprintf("%s %s", p.Integration, p.Status),
		"text":    text,
		"sections": []map[string]any{
			{"facts": facts},
		},
	}
}

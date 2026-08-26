package notify

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/curruwilla/vaultd/internal/core"
)

// Template picks the shape of the JSON body a webhook receives. `generic` is
// the documented vaultd payload (SPEC §12); the other two are the native
// formats of the two chat services people actually point this at, because a
// Slack incoming webhook rejects anything it does not recognise.
type Template string

const (
	TemplateGeneric Template = "generic"
	TemplateSlack   Template = "slack"
	TemplateDiscord Template = "discord"
)

// Templates lists every supported rendering.
var Templates = []Template{TemplateGeneric, TemplateSlack, TemplateDiscord}

// Render turns a notification into the body that goes on the wire.
func (t Template) Render(n core.Notification) ([]byte, error) {
	var (
		payload any
		err     error
	)

	switch t {
	case TemplateSlack:
		payload = slackPayload(n)
	case TemplateDiscord:
		payload = discordPayload(n)
	case TemplateGeneric, "":
		payload = n
	default:
		return nil, fmt.Errorf("unknown template %q; use one of %s", t, joinTemplates())
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding the %s payload: %w", t, err)
	}
	return body, nil
}

// slack colours are the three Slack understands by name.
func slackColor(s core.Severity) string {
	switch s {
	case core.SeverityCritical:
		return "danger"
	case core.SeverityWarning:
		return "warning"
	default:
		return "good"
	}
}

// discord colours are RGB integers, which is the only form its API takes.
func discordColor(s core.Severity) int {
	switch s {
	case core.SeverityCritical:
		return 0xD64545 // red
	case core.SeverityWarning:
		return 0xD6A245 // amber
	default:
		return 0x3FA45B // green
	}
}

func slackPayload(n core.Notification) map[string]any {
	attachment := map[string]any{
		"color":     slackColor(n.Severity),
		"title":     string(n.Event),
		"text":      n.Summary,
		"fallback":  n.Summary,
		"ts":        n.At.Unix(),
		"fields":    slackFields(n),
		"mrkdwn_in": []string{"text"},
	}

	return map[string]any{
		"text":        n.Summary,
		"attachments": []any{attachment},
	}
}

func slackFields(n core.Notification) []map[string]any {
	fields := make([]map[string]any, 0, 4)
	for _, f := range fieldsOf(n) {
		fields = append(fields, map[string]any{
			"title": f.title,
			"value": f.value,
			"short": len(f.value) < 40,
		})
	}
	return fields
}

func discordPayload(n core.Notification) map[string]any {
	fields := make([]map[string]any, 0, 4)
	for _, f := range fieldsOf(n) {
		fields = append(fields, map[string]any{
			"name":   f.title,
			"value":  f.value,
			"inline": len(f.value) < 40,
		})
	}

	embed := map[string]any{
		"title":       string(n.Event),
		"description": n.Summary,
		"color":       discordColor(n.Severity),
		"timestamp":   n.At.UTC().Format(time.RFC3339),
		"fields":      fields,
	}

	return map[string]any{
		"content": n.Summary,
		"embeds":  []any{embed},
	}
}

// field is one labelled value in a chat rendering.
type field struct {
	title string
	value string
}

// fieldsOf is what both chat templates put under the summary. The error and
// its stderr tail come last and longest: it is what an operator scrolls to.
func fieldsOf(n core.Notification) []field {
	fields := make([]field, 0, 6)

	if n.Target != "" {
		fields = append(fields, field{"target", n.Target})
	}
	if n.BackupID != "" {
		fields = append(fields, field{"backup", n.BackupID})
	}
	if n.DurationMS > 0 {
		fields = append(fields, field{"duration", (time.Duration(n.DurationMS) * time.Millisecond).String()})
	}
	fields = append(fields, field{"severity", string(n.Severity)})

	for _, key := range sortedKeys(n.Details) {
		fields = append(fields, field{key, fmt.Sprint(n.Details[key])})
	}

	if n.Error != nil {
		if n.Error.Phase != "" {
			fields = append(fields, field{"phase", n.Error.Phase})
		}
		fields = append(fields, field{"error", n.Error.Message})
		if tail := n.Error.StderrTail; tail != "" {
			fields = append(fields, field{"stderr", "```\n" + lastLines(tail, 10) + "\n```"})
		}
	}
	return fields
}

// sortedKeys keeps a rendering stable: a map iterated at random would make two
// deliveries of the same event look different.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// lastLines trims a stderr tail to something a chat message can hold. The
// manifest and the generic payload keep all 64KB of it.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return "…\n" + strings.Join(lines[len(lines)-n:], "\n")
}

func joinTemplates() string {
	names := make([]string, 0, len(Templates))
	for _, t := range Templates {
		names = append(names, string(t))
	}
	return strings.Join(names, ", ")
}

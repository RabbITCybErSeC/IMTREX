package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel is the generic webhook adapter: the user supplies the URL, method, headers and JSON template.
// It is what keeps this feature from needing a separate implementation for Slack / Mattermost / Discord /
// in-house systems -- all of those are covered by one configurable template.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// A generic webhook has no official limit, so 0 means no rate limiting by default, left for the user to set from the peer's capacity.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url and headers are masked: the destination address often carries a token and custom headers usually
// hold authentication credentials, and both appear in the API echo, so both must be blocked.
// The cost is that changing one header while editing means re-entering the whole header set (a masked
// value is read as "keep the original") -- a deliberate trade-off: better to fill it in once more than to echo credentials to the browser.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// The destination is url. Changing url requires restating headers -- otherwise the original
// Authorization header would be sent verbatim to the new address, which is the main mask-bypass path.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate is the fallback request body when no template is supplied: a plain JSON
// structure that covers the vast majority of in-house receivers that just "take a JSON and store it".
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData is the context exposed to the user's template.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt is the delivery time (RFC3339), for the receiver to record.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel is the readable description of a status change, e.g. "Pending -> Fixed"; empty when it is not a status change.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("the target URL is missing")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("invalid target URL: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("unsupported method %s (use GET/POST/PUT/PATCH)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("syntax error in the request body template: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET carries no request body: stuffing the content into the query is beyond what a template can do
	// and does not match GET semantics, so GET only suits "a hit triggers the hook" style receivers.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// The template renders JSON as a string, which is converted to json.RawMessage and sent as-is,
		// so a second round of escaping does not wrap the user's carefully built structure inside a JSON string.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("the rendered request body template is not valid JSON"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// Overriding is allowed, but applied after headers so an explicit setting wins.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// A generic webhook does not truncate the body (the receiver is the user's own service and the size
	// is decided by body_template), so the whole batch counts as delivered.
	return len(m.Items), nil
}

// renderWebhookBody renders the request body with the user's template (or the default one).
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("syntax error in the request body template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("failed to render the request body template: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate parses a template.
//
// missingkey=zero makes a missing map key render as the zero value instead of erroring -- though the
// context in this file is a struct, so its main effect is keeping a range over an empty .Items from failing. What really needs guarding against is a nil .Items.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs are the helper functions exposed to templates.
var webhookTemplateFuncs = template.FuncMap{
	// json serializes an arbitrary value as JSON.
	//
	// This function is a necessity rather than a nicety: without it a user can only write {{.Title}} and
	// interpolate directly, and a single quote or newline in a finding title makes the whole request body
	// invalid JSON -- the receiver rejects it, and the error points at "JSON parse failure", with nothing to suggest a quote in a title was the cause.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons embeds a JSON fragment inside another JSON string value (adding one layer of string escaping).
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// Strip the outer quotes: the caller decides whether to add their own.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}

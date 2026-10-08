package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file pins down the **capability boundary** of the generic webhook template.
//
// It is the only place in the package where a user-supplied string is evaluated as code, so what it
// can and cannot do is stated explicitly and fixed by tests -- otherwise someone could casually add a
// method to the template context, or a readFile to the FuncMap, and silently widen the surface while the diff looks like one harmless little function.

// TestTemplateContextHasNoMethods is the most important one.
//
// text/template calls exported methods ({{.Foo}} reads a field or calls a method). So if the template
// context can reach **any** type with exported methods, those methods are exposed to whoever writes the template.
// This feature's context is deliberately pure data (exported fields only, zero methods).
//
// If this fails: someone added a method to webhookTemplateData / webhookItem.
// Before letting it through, work out whether that method could be used from a template to read something that should not be exposed.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d method(s) (%s): text/template can call them, "+
				"which hands their capabilities to whoever writes the template", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal pins down the set of functions exposed to templates.
//
// Every extra function in the FuncMap is an extra capability. Right now there are only json / jsons,
// which serialize a value into a JSON fragment -- they cannot read files, make requests or run commands.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the template function set changed: got %v, want %v. Before adding a function, confirm it does not widen the capability surface"+
			" (no reading or writing files, no network requests, no running commands)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds access in a template:
// reaching something that does not exist must fail rather than echo anything, and the failure message must not carry internal data out.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a non-existent field should error")
	}
	// The error must not contain the real content of the template context (the finding title/summary).
	for _, leak := range []string{"SQL injection", "parameter id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the template error leaked message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: a broken template is a configuration error that retrying will not heal.
// Classed as retryable, one bad template would make every delivery waste three rounds of backoff.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("a template syntax error should already be caught on save")
	}
	// Even if validation is bypassed and it is delivered directly, it must be a permanent failure rather than retried repeatedly.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a bad template should be a permanent failure, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON covers the "a rendered template must be valid JSON" constraint.
// It also blocks using a template to produce plain text in order to trigger some other protocol.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// A valid template passes.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("a valid template should pass validation: %v", err)
	}
	// Rendering something that is not JSON must be rejected (rather than sent as-is).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("rendering non-JSON should be a permanent failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("the error message should say it is a JSON problem, got %v", err)
	}
}

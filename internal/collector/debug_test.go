package collector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kentrow/tapyr/internal/api"
	"github.com/kentrow/tapyr/internal/collect"
	"github.com/kentrow/tapyr/internal/mapping"
	"github.com/kentrow/tapyr/internal/model"
	"github.com/kentrow/tapyr/internal/runtime"
)

func TestDebugCaptures(t *testing.T) {
	d := newDebugCaptures(func(s string) string { return strings.ReplaceAll(s, "s3cr3t", "[REDACTED]") })
	at := time.Date(2026, time.November, 3, 9, 0, 0, 0, time.UTC)
	res := &collect.Result{Pages: []mapping.Page{{
		Status: 200, Headers: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"sid=1"}},
		Body: map[string]any{"token": "s3cr3t", "items": []any{int64(1)}},
	}}}
	samples := []model.Sample{{Name: "up", Type: model.Gauge, Labels: model.Labels{{Name: "k", Value: "s3cr3t"}}, Value: 1}}
	job := func(name, instance string) *runtime.Job { return &runtime.Job{Name: name, Instance: instance} }

	d.keep(job("devices", "devices"), "1", at, "success", "", res, samples, debugHeaders, debugFailure{})
	if _, err := d.get(context.Background(), "devices", ""); !errors.Is(err, api.ErrDebugDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	d.configure(true, 0)
	d.keep(job("devices", "devices[a]"), "1", at, "success", "", res, samples, debugHeaders, debugFailure{})
	d.keep(job("devices", "devices[b]"), "2", at, "failed", "boom s3cr3t", nil, nil, debugHeaders, debugFailure{})
	c, err := d.get(context.Background(), "devices", "")
	if err != nil || c.Instance != "devices[b]" || c.Error != "boom [REDACTED]" || len(c.Pages) != 0 || strings.Join(c.Instances, ",") != "devices[a],devices[b]" {
		t.Fatalf("latest = %+v, %v", c, err)
	}
	c, err = d.get(context.Background(), "devices", "devices[a]")
	if err != nil || len(c.Pages) != 1 || strings.Contains(c.Pages[0].Body, "s3cr3t") || c.Pages[0].Headers["Set-Cookie"] != "" ||
		c.Pages[0].Headers["Content-Type"] != "application/json" || c.Samples[0].Labels["k"] != "[REDACTED]" || c.Samples[0].Type != "gauge" {
		t.Fatalf("instance a = %+v, %v", c, err)
	}
	// A failed request keeps its response, redacted, with the problem.
	failed := &collect.Error{
		Class: collect.ClassAuth, Endpoint: "/sites", Status: 401, Err: errors.New("response: denied"),
		Body: []byte(`{"error":"invalid key s3cr3t"}`), Header: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"sid=1"}},
	}
	prob := &api.Problem{Class: "auth", Title: "The provider rejected the credentials of fleet."}
	d.keep(job("devices", "devices[c]"), "3", at, "failed", "auth /sites: HTTP 401", nil, nil, debugHeaders, debugFailure{problem: prob, request: failed})
	c, err = d.get(context.Background(), "devices", "devices[c]")
	if err != nil || c.Problem != prob || len(c.Pages) != 1 || c.Pages[0].Status != 401 || c.Pages[0].Headers["Set-Cookie"] != "" ||
		c.Pages[0].Body != "{\n  \"error\": \"invalid key [REDACTED]\"\n}" {
		t.Fatalf("failed run = %+v, %v", c, err)
	}
	// A request that failed before any response keeps no page.
	d.keep(job("devices", "devices[d]"), "4", at, "failed", "transient /sites: timeout", nil, nil, debugHeaders,
		debugFailure{request: &collect.Error{Class: collect.ClassTransient, Endpoint: "/sites", Err: errors.New("timeout")}})
	if c, err = d.get(context.Background(), "devices", "devices[d]"); err != nil || len(c.Pages) != 0 {
		t.Fatalf("no response = %+v, %v", c, err)
	}
	d.remove("devices[c]")
	d.remove("devices[d]")
	d.remove("devices[a]")
	if _, err := d.get(context.Background(), "devices", "devices[a]"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("removed: %v", err)
	}
	// Truncation, and the oldest captures go first.
	d.configure(true, 10)
	for i := range maxDebugCaptures + 5 {
		d.keep(job("fan", fmt.Sprintf("fan[%03d]", i)), "1", at, "success", "", res, nil, debugHeaders, debugFailure{})
	}
	if _, err := d.get(context.Background(), "fan", "fan[000]"); !errors.Is(err, api.ErrNotFound) {
		t.Error("the oldest capture was kept")
	}
	c, err = d.get(context.Background(), "fan", "fan[204]")
	if err != nil || len(c.Pages[0].Body) != 10 || !c.Pages[0].Truncated || len(c.Instances) != maxDebugCaptures {
		t.Errorf("latest fan = %+v, %v", c, err)
	}
	d.configure(false, 0)
	d.configure(true, 0)
	if _, err := d.get(context.Background(), "fan", ""); !errors.Is(err, api.ErrNotFound) {
		t.Error("disabling did not drop the captures")
	}
}

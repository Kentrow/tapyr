package sim

import (
	"encoding/json"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/kentrow/tapyr/internal/mapping"
)

const recorded = `{
  "success": true,
  "data": [
    {"id": 70810, "company_id": 7034, "company_name": "Acme Networks", "name": "North router",
     "serial": "1118789105", "mac": "0A:1B:2C:3D:4E:5F", "imei": "356000000000001", "wan_ip": "100.64.12.34",
     "firmware": "RUT9XX_R_00.07.06.1", "firmware_version": "7.3.0.1", "model": "RUT950", "status": 1,
     "lat": 12.3456, "lng": 65.4321, "email": "noc@acme.example.org",
     "notes": "gateway 100.64.12.34, contact noc@acme.example.org, peer fd12:3456:789a::8888",
     "tags": [{"id": 11, "name": "North"}], "signal": -81, "networkId": "N_24329156"}
  ],
  "meta": {"total": 1}
}`

func TestAnonymizer(t *testing.T) {
	doc, err := mapping.DecodeJSON([]byte(recorded))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAnonymizer(AnonymizeRules{
		Replace: []PathRule{{Path: "data[*].model", As: RuleRedact}},
		Keep:    []string{"data[*].tags[*].name"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := a.Document(doc)
	data, _ := json.Marshal(out)
	text := string(data)
	leaves := scalarLeaves(out)
	for _, original := range []string{
		"70810", "7034", "Acme Networks", "North router", "1118789105", "0A:1B:2C:3D:4E:5F", "356000000000001",
		"100.64.12.34", "noc@acme.example.org", "12.3456", "65.4321", "fd12:3456:789a::8888", "N_24329156", "RUT950",
	} {
		// Text can hide inside free text (notes); a number can show up by
		// chance among the digits of a pseudonym, so it must only not be
		// a value of its own.
		_, numeric := strconv.ParseFloat(original, 64)
		for _, leaf := range leaves {
			if leaf == original || numeric != nil && strings.Contains(leaf, original) {
				t.Errorf("%s is still in the document: %s", original, text)
			}
		}
	}
	d := out.(map[string]any)["data"].([]any)[0].(map[string]any)
	// Kept as they are: versions, statuses, measures, kept paths.
	for k, want := range map[string]any{
		"firmware": "RUT9XX_R_00.07.06.1", "firmware_version": "7.3.0.1", "status": int64(1), "signal": int64(-81), "model": "redacted",
	} {
		if d[k] != want {
			t.Errorf("%s = %v, want %v", k, d[k], want)
		}
	}
	if tag := d["tags"].([]any)[0].(map[string]any); tag["name"] != "North" || tag["id"] == int64(11) {
		t.Errorf("tag = %v", tag)
	}
	// Formats are kept.
	if id, ok := d["id"].(int64); !ok || id < 10000 || id > 99999 {
		t.Errorf("id = %v", d["id"])
	}
	if s := d["serial"].(string); !regexp.MustCompile(`^[1-9]\d{9}$`).MatchString(s) {
		t.Errorf("serial = %s", s)
	}
	if m := d["mac"].(string); !regexp.MustCompile(`^02(:[0-9A-F]{2}){5}$`).MatchString(m) {
		t.Errorf("mac = %s", m)
	}
	if ip := net.ParseIP(d["wan_ip"].(string)); ip == nil || !strings.HasPrefix(ip.String(), "192.0.2.") &&
		!strings.HasPrefix(ip.String(), "198.51.100.") && !strings.HasPrefix(ip.String(), "203.0.113.") {
		t.Errorf("wan_ip = %v", d["wan_ip"])
	}
	if e := d["email"].(string); !strings.HasSuffix(e, "@example.com") {
		t.Errorf("email = %s", e)
	}
	if n := d["networkId"].(string); !regexp.MustCompile(`^[A-Z]_\d{8}$`).MatchString(n) {
		t.Errorf("networkId = %s", n)
	}
	notes := d["notes"].(string)
	if !strings.Contains(notes, d["wan_ip"].(string)) || !strings.Contains(notes, d["email"].(string)) || !strings.Contains(notes, "2001:db8:") {
		t.Errorf("notes = %s", notes)
	}
	if lat, ok := d["lat"].(float64); !ok || lat < -80 || lat > 80 {
		t.Errorf("lat = %v", d["lat"])
	}
	// Consistent: the same value gives the same pseudonym, in documents and
	// in request variables.
	if v := a.Var("networkId", "N_24329156"); v != d["networkId"] {
		t.Errorf("Var(networkId) = %s, document %s", v, d["networkId"])
	}
	if v := a.Var("deviceId", "70810"); v != formatInt(d["id"]) {
		t.Errorf("Var(deviceId) = %s, document %v", v, d["id"])
	}
	if v := a.Var("baseURL", "https://100.64.12.34/api"); !strings.HasPrefix(v, "https://") || !strings.HasSuffix(v, "/api") || strings.Contains(v, "100.64.12.34") {
		t.Errorf("Var(baseURL) = %s", v)
	}
	// The report names paths and rules, never values.
	report := a.Report()
	var lines []string
	for _, r := range report {
		lines = append(lines, r.Path+" "+string(r.Rule))
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"data[*].serial serial", "data[*].notes pattern", "data[*].model redact", "data[*].company_id id", "vars.networkId id"} {
		if !strings.Contains(joined, want) {
			t.Errorf("report does not contain %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "70810") {
		t.Error("the report holds a value")
	}
	// Another recording uses another key.
	b, _ := NewAnonymizer(AnonymizeRules{})
	if b.str(RuleSerial, "1118789105") == a.str(RuleSerial, "1118789105") {
		t.Error("two recordings share their key")
	}
	if _, err := NewAnonymizer(AnonymizeRules{Replace: []PathRule{{Path: "x", As: "nope"}}}); err == nil {
		t.Error("unknown rule accepted")
	}
}

// TestAnonymizerSiteAndZipcode is a regression test: a recording of Aruba
// Central kept the site name of every device (key site) and the zip code of
// the sites (key zipcode).
func TestAnonymizerSiteAndZipcode(t *testing.T) {
	doc, err := mapping.DecodeJSON([]byte(`{
	  "aps": [{"serial": "CN0000001", "site": "North campus", "status": "Up"}],
	  "sites": [{"site_id": 7, "site_name": "North campus", "zipcode": "99999", "state": "online"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAnonymizer(AnonymizeRules{})
	if err != nil {
		t.Fatal(err)
	}
	out := a.Document(doc).(map[string]any)
	ap := out["aps"].([]any)[0].(map[string]any)
	site := out["sites"].([]any)[0].(map[string]any)
	if ap["site"] == "North campus" || site["zipcode"] == "99999" {
		t.Fatalf("site or zipcode kept: %v %v", ap, site)
	}
	// The site of a device keeps pointing at its site.
	if ap["site"] != site["site_name"] {
		t.Errorf("site = %v, site_name = %v", ap["site"], site["site_name"])
	}
	// state is often a status (online, Up): it is left to the review.
	if ap["status"] != "Up" || site["state"] != "online" {
		t.Errorf("status or state changed: %v %v", ap, site)
	}
}

// TestPseudonymNeverEqualsValue checks every value of up to 3 digits, and
// short letter values, under many keys: none comes out unchanged.
func TestPseudonymNeverEqualsValue(t *testing.T) {
	values := []string{"a", "Z", "ab", "A-1", "0", "00"}
	for n := range 1000 {
		values = append(values, strconv.Itoa(n))
	}
	for k := range 50 {
		a, err := NewAnonymizer(AnonymizeRules{})
		if err != nil {
			t.Fatal(err)
		}
		a.key = []byte{byte(k)}
		for _, v := range values {
			if out := a.str(RuleDigits, v); out == v || len(out) != len(v) {
				t.Fatalf("key %d: %q -> %q", k, v, out)
			}
		}
	}
	a, _ := NewAnonymizer(AnonymizeRules{})
	if out := a.str(RuleDigits, "-.:"); out != "-.:" {
		t.Errorf("a value without digits or letters changed: %q", out)
	}
}

// TestPseudonymReplacesNonASCII checks that letters and digits outside ASCII
// are replaced too: found by FuzzAnonymizer, a serial "덻" was kept.
func TestPseudonymReplacesNonASCII(t *testing.T) {
	a, err := NewAnonymizer(AnonymizeRules{})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"덻", "Сер-123", "Ünïcode-9", "١٢٣", "ab-ÉÀ"} {
		for _, rule := range []Rule{RuleSerial, RuleID} {
			out := a.str(rule, v)
			if out == v || utf8.RuneCountInString(out) != utf8.RuneCountInString(v) {
				t.Errorf("%s %q -> %q", rule, v, out)
			}
			for _, r := range out {
				if r > unicode.MaxASCII {
					t.Errorf("%s %q -> %q keeps %q", rule, v, out, r)
				}
			}
			if strings.Contains(v, "-") != strings.Contains(out, "-") {
				t.Errorf("%s %q -> %q lost the punctuation", rule, v, out)
			}
		}
	}
}

func formatInt(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// scalarLeaves returns the strings and numbers of a decoded document, as
// written in JSON.
func scalarLeaves(v any) []string {
	switch v := v.(type) {
	case map[string]any:
		var out []string
		for _, e := range v {
			out = append(out, scalarLeaves(e)...)
		}
		return out
	case []any:
		var out []string
		for _, e := range v {
			out = append(out, scalarLeaves(e)...)
		}
		return out
	case string:
		return []string{v}
	case int64:
		return []string{strconv.FormatInt(v, 10)}
	case float64:
		return []string{strconv.FormatFloat(v, 'f', -1, 64)}
	}
	return nil
}

package sim

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Rule is a kind of pseudonym of the anonymizer.
type Rule string

// Anonymization rules (see docs/spec/08-simulator.md, "Record mode").
const (
	RuleSerial     Rule = "serial"     // same length and character classes
	RuleDigits     Rule = "digits"     // IMEI, IMSI, ICCID, phone numbers
	RuleMAC        Rule = "mac"        // locally administered MAC
	RuleIPv4       Rule = "ipv4"       // documentation ranges
	RuleIPv6       Rule = "ipv6"       // 2001:db8::/32
	RuleEmail      Rule = "email"      // example.com
	RuleHostname   Rule = "hostname"   // host-xxxxxx
	RuleName       Rule = "name"       // name-xxxxxx
	RuleID         Rule = "id"         // same length and character classes
	RuleCoordinate Rule = "coordinate" // a number in [-80, 80]
	RuleAddress    Rule = "address"    // address-xxxxxx
	RuleRedact     Rule = "redact"     // "redacted"
)

// rules lists the rules accepted in anonymization files.
var rules = []Rule{
	RuleSerial, RuleDigits, RuleMAC, RuleIPv4, RuleIPv6, RuleEmail, RuleHostname, RuleName, RuleID,
	RuleCoordinate, RuleAddress, RuleRedact,
}

// AnonymizeRules are the rules added by an anonymization file: replaced
// paths with their rule, and kept paths. Paths are dotted, with [*] for
// every element of a list (data[*].serial).
type AnonymizeRules struct {
	Replace []PathRule `json:"replace,omitempty"`
	Keep    []string   `json:"keep,omitempty"`
}

// PathRule replaces the values at a path.
type PathRule struct {
	Path string `json:"path"`
	As   Rule   `json:"as"`
}

// Anonymizer replaces identifying values by consistent pseudonyms: the
// same input gives the same pseudonym within a recording, from an HMAC with
// a random key that is never written.
type Anonymizer struct {
	key     []byte
	replace map[string]Rule
	keep    map[string]bool
	// Report counts the replacements by path and rule.
	report map[[2]string]int
}

// NewAnonymizer returns an anonymizer with the default rules and extra.
func NewAnonymizer(extra AnonymizeRules) (*Anonymizer, error) {
	a := &Anonymizer{key: make([]byte, 32), replace: map[string]Rule{}, keep: map[string]bool{}, report: map[[2]string]int{}}
	if _, err := rand.Read(a.key); err != nil {
		return nil, err
	}
	for _, r := range extra.Replace {
		if !slices.Contains(rules, r.As) {
			return nil, fmt.Errorf("replace %s: unknown rule %q", r.Path, r.As)
		}
		a.replace[r.Path] = r.As
	}
	for _, p := range extra.Keep {
		a.keep[p] = true
	}
	return a, nil
}

// Replacement is a line of the recording report.
type Replacement struct {
	Path  string
	Rule  Rule
	Count int
}

// Report lists the replacements, sorted by path and rule. It never holds an
// original value.
func (a *Anonymizer) Report() []Replacement {
	out := make([]Replacement, 0, len(a.report))
	for k, n := range a.report {
		out = append(out, Replacement{Path: k[0], Rule: Rule(k[1]), Count: n})
	}
	slices.SortFunc(out, func(x, y Replacement) int {
		if c := strings.Compare(x.Path, y.Path); c != 0 {
			return c
		}
		return strings.Compare(string(x.Rule), string(y.Rule))
	})
	return out
}

// Document anonymizes a decoded JSON document (as decoded by
// mapping.DecodeJSON) and returns the copy.
func (a *Anonymizer) Document(doc any) any { return a.walk(doc, "", "") }

// Var anonymizes a request variable (an identifier from the inventory), the
// way an identifier of a document is, so that fixtures stay consistent.
func (a *Anonymizer) Var(name, value string) string {
	rule, ok := keyRule(name)
	if !ok {
		// Addresses inside a value (a base URL) are replaced in place.
		if out, n := a.patterns(value); n > 0 {
			a.report[[2]string{"vars." + name, "pattern"}] += n
			return out
		}
		rule = RuleID
	}
	v := a.apply(rule, value)
	a.report[[2]string{"vars." + name, string(rule)}]++
	return fmt.Sprint(v)
}

func (a *Anonymizer) walk(v any, path, key string) any {
	if a.keep[path] {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = a.walk(val, join(path, k), k)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = a.walk(val, path+"[*]", key)
		}
		return out
	case nil, bool:
		return v
	}
	rule, ok := a.replace[path]
	if !ok {
		rule, ok = keyRule(key)
	}
	if ok {
		a.report[[2]string{path, string(rule)}]++
		return a.apply(rule, v)
	}
	if s, isString := v.(string); isString && !versionKey(key) {
		if out, n := a.patterns(s); n > 0 {
			a.report[[2]string{path, "pattern"}] += n
			return out
		}
	}
	return v
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// keyRule returns the default rule of a key, by its name.
func keyRule(key string) (Rule, bool) {
	k := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	switch {
	case k == "":
		return "", false
	case strings.Contains(k, "serial"):
		return RuleSerial, true
	case k == "mac" || strings.HasSuffix(k, "mac") || strings.HasPrefix(k, "macaddr") || k == "bssid":
		return RuleMAC, true
	case slices.Contains([]string{"imei", "imsi", "iccid", "msisdn", "phone", "phonenumber"}, k):
		return RuleDigits, true
	case strings.Contains(k, "ipv6"):
		return RuleIPv6, true
	case k == "ip" || strings.HasSuffix(k, "ip") || strings.Contains(k, "ipaddress") || strings.Contains(k, "ipv4") ||
		k == "gateway" || strings.HasSuffix(k, "dns"):
		return RuleIPv4, true
	case k == "hostname" || k == "host" || k == "fqdn":
		return RuleHostname, true
	case strings.Contains(k, "email"):
		return RuleEmail, true
	case slices.Contains([]string{"lat", "lng", "lon", "latitude", "longitude"}, k):
		return RuleCoordinate, true
	case strings.Contains(k, "address") || slices.Contains([]string{"street", "city", "zip", "zipcode", "postcode", "postalcode"}, k):
		return RuleAddress, true
	// A site is usually referenced by its name (Aruba Central devices).
	case k == "name" || strings.HasSuffix(k, "name") || k == "site":
		return RuleName, true
	case k == "id" || strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "Id") || strings.HasSuffix(key, "ID") ||
		k == "uuid" || k == "cellid":
		return RuleID, true
	}
	return "", false
}

// versionKey reports keys whose values look like addresses but are
// versions (7.3.0.1).
func versionKey(key string) bool {
	k := strings.ToLower(key)
	return strings.Contains(k, "version") || strings.Contains(k, "firmware") || strings.HasPrefix(k, "fw")
}

var (
	macPattern   = regexp.MustCompile(`\b[0-9A-Fa-f]{2}([:-])(?:[0-9A-Fa-f]{2}[:-]){4}[0-9A-Fa-f]{2}\b`)
	ipv4Pattern  = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	ipv6Pattern  = regexp.MustCompile(`\b[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}\b`)
)

// patterns replaces the MAC, IP and email addresses inside a string, and
// returns the number of replacements.
func (a *Anonymizer) patterns(s string) (string, int) {
	n := 0
	s = emailPattern.ReplaceAllStringFunc(s, func(m string) string { n++; return a.str(RuleEmail, m) })
	s = macPattern.ReplaceAllStringFunc(s, func(m string) string { n++; return a.str(RuleMAC, m) })
	s = ipv4Pattern.ReplaceAllStringFunc(s, func(m string) string {
		if net.ParseIP(m) == nil {
			return m
		}
		n++
		return a.str(RuleIPv4, m)
	})
	s = ipv6Pattern.ReplaceAllStringFunc(s, func(m string) string {
		if ip := net.ParseIP(m); ip == nil || ip.To4() != nil || !strings.Contains(m, "::") && strings.Count(m, ":") < 7 {
			return m
		}
		n++
		return a.str(RuleIPv6, m)
	})
	return s, n
}

// sum returns the HMAC of a value.
func (a *Anonymizer) sum(v string) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(v))
	return m.Sum(nil)
}

func (a *Anonymizer) hex6(v string) string { return fmt.Sprintf("%x", a.sum(v)[:3]) }

// apply replaces a value by its pseudonym; numbers stay numbers.
func (a *Anonymizer) apply(rule Rule, v any) any {
	switch x := v.(type) {
	case int64:
		if rule == RuleCoordinate {
			f, _ := a.coordinate(strconv.FormatInt(x, 10)).Float64()
			return f
		}
		n, err := strconv.ParseInt(a.str(RuleDigits, strconv.FormatInt(x, 10)), 10, 64)
		if err != nil {
			return int64(0)
		}
		return n
	case json.Number:
		if rule == RuleCoordinate {
			return a.coordinate(x.String())
		}
		out := a.str(RuleDigits, x.String())
		if _, err := strconv.ParseFloat(out, 64); err == nil {
			return json.Number(out)
		}
		return json.Number("0")
	case float64:
		if rule == RuleCoordinate {
			f, _ := a.coordinate(strconv.FormatFloat(x, 'g', -1, 64)).Float64()
			return f
		}
		f, err := strconv.ParseFloat(a.str(RuleDigits, strconv.FormatFloat(x, 'f', -1, 64)), 64)
		if err != nil {
			return 0.0
		}
		return f
	case string:
		return a.str(rule, x)
	}
	return a.str(rule, fmt.Sprint(v))
}

func (a *Anonymizer) coordinate(v string) json.Number {
	u := binary.BigEndian.Uint64(a.sum("coordinate:" + v)[:8])
	f := float64(u)/math.MaxUint64*160 - 80
	return json.Number(strconv.FormatFloat(math.Round(f*1e4)/1e4, 'f', -1, 64))
}

// str returns the pseudonym of a string.
func (a *Anonymizer) str(rule Rule, v string) string {
	if v == "" {
		return v
	}
	h := a.sum(v)
	switch rule {
	case RuleMAC:
		sep := ":"
		if strings.Contains(v, "-") {
			sep = "-"
		}
		out := fmt.Sprintf("02%s%02x%s%02x%s%02x%s%02x%s%02x", sep, h[0], sep, h[1], sep, h[2], sep, h[3], sep, h[4])
		if strings.ToUpper(v) == v {
			out = strings.ToUpper(out)
		}
		return out
	case RuleIPv4:
		nets := []string{"192.0.2", "198.51.100", "203.0.113"}
		return fmt.Sprintf("%s.%d", nets[int(h[0])%len(nets)], 1+int(h[1])%254)
	case RuleIPv6:
		return fmt.Sprintf("2001:db8:%x:%x::%x", binary.BigEndian.Uint16(h[0:2]), binary.BigEndian.Uint16(h[2:4]), binary.BigEndian.Uint16(h[4:6]))
	case RuleEmail:
		return "user-" + a.hex6(v) + "@example.com"
	case RuleHostname:
		return "host-" + a.hex6(v)
	case RuleName:
		return "name-" + a.hex6(v)
	case RuleAddress:
		return "address-" + a.hex6(v)
	case RuleRedact:
		return "redacted"
	case RuleCoordinate:
		return a.coordinate(v).String()
	}
	return a.classes(v, h)
}

// classes keeps the length and the character classes of a value: digits,
// upper and lower case letters (ASCII or not) are replaced by ASCII ones,
// other characters kept. A
// leading digit stays non-zero, so that numbers keep their length. The
// pseudonym never equals the value: a 3-digit serial would otherwise come
// out unchanged once in 900 recordings.
func (a *Anonymizer) classes(v string, h []byte) string {
	if !strings.ContainsFunc(v, func(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }) {
		return v
	}
	out := a.substitute(v, h)
	for i := 0; out == v && i < 64; i++ {
		h = a.sum(string(h))
		out = a.substitute(v, h)
	}
	return out
}

// substitute replaces the digits and letters of a value from the stream
// of h, character by character: letters and digits outside ASCII become
// ASCII ones of the same class, so that no character of the value is kept.
func (a *Anonymizer) substitute(v string, h []byte) string {
	const digits, upper, lower = "0123456789", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "abcdefghijklmnopqrstuvwxyz"
	stream := h
	runes := []rune(v)
	for i, c := range runes {
		if i%len(h) == 0 && i > 0 {
			stream = a.sum(string(stream))
		}
		r := int(stream[i%len(stream)])
		switch {
		case unicode.IsDigit(c):
			if i == 0 && c != '0' {
				runes[i] = rune(digits[1+r%9])
			} else {
				runes[i] = rune(digits[r%10])
			}
		case unicode.IsUpper(c):
			runes[i] = rune(upper[r%26])
		case unicode.IsLetter(c):
			runes[i] = rune(lower[r%26])
		}
	}
	return string(runes)
}

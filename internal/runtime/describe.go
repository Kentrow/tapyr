package runtime

import (
	"slices"
	"strings"

	"github.com/kentrow/tapyr/internal/config"
)

// describeImplicit names, for every implicit job, the items it lists and
// the jobs of the configuration it lists them for, so that people see
// "networks, for demo-network-devices" rather than
// demo.example-sim-networks alone.
func (c *compiler) describeImplicit() {
	byName := map[string]*Job{}
	for _, j := range c.graph.Jobs {
		if _, ok := byName[j.Name]; !ok {
			byName[j.Name] = j
		}
	}
	// The declared jobs below every implicit job, through the implicit
	// jobs between them.
	for name, j := range byName {
		if j.Implicit {
			continue
		}
		seen := map[string]bool{}
		for p := j; p.ForEach != nil && p.ForEach.ParentJob != ""; {
			parent := byName[p.ForEach.ParentJob]
			if parent == nil || !parent.Implicit || seen[parent.Name] {
				break
			}
			seen[parent.Name] = true
			parent.DiscoveryFor = append(parent.DiscoveryFor, name)
			p = parent
		}
	}
	prefixes := map[string]string{}
	for _, j := range c.graph.Jobs {
		if !j.Implicit {
			continue
		}
		first := byName[j.Name]
		if first != j {
			// A model per target: they share the description.
			j.DiscoveryFor, j.Items = first.DiscoveryFor, first.Items
			continue
		}
		slices.Sort(j.DiscoveryFor)
		prefix, ok := prefixes[j.Provider]
		if !ok {
			prefix = c.templatePrefix(config.Name(j.Provider))
			prefixes[j.Provider] = prefix
		}
		j.Items = itemWords(j.Template, prefix)
	}
}

// templatePrefix returns the start that the template names of a provider
// share, up to a hyphen ("example-sim-", "meraki-"), or "" when they share
// none or the provider has a single template.
func (c *compiler) templatePrefix(provider config.Name) string {
	var names []string
	for name, t := range c.templates {
		if t.Spec.Provider == provider {
			names = append(names, string(name))
		}
	}
	if len(names) < 2 {
		return ""
	}
	prefix := names[0]
	for _, n := range names[1:] {
		for !strings.HasPrefix(n, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	if i := strings.LastIndex(prefix, "-"); i >= 0 {
		return prefix[:i+1]
	}
	return ""
}

// itemWords names the items a template lists from its name:
// "example-sim-networks" with the prefix "example-sim-" is "networks",
// "meraki-organizations" with "meraki-" is "organizations".
func itemWords(template, prefix string) string {
	words := strings.TrimPrefix(template, prefix)
	if words == "" {
		words = template
	}
	return strings.ReplaceAll(words, "-", " ")
}

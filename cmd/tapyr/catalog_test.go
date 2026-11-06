package main

import (
	"regexp"
	"testing"
)

func TestCatalogCategories(t *testing.T) {
	cats, err := catalogCategories()
	if err != nil {
		t.Fatal(err)
	}
	if len(cats) == 0 {
		t.Fatal("no category")
	}
	name := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	seen := map[string]bool{}
	for _, c := range cats {
		if !name.MatchString(string(c.Name)) || c.Description == "" {
			t.Errorf("category %+v: a name is a DNS label and a description is required", c)
		}
		if seen[string(c.Name)] {
			t.Errorf("category %s is listed twice", c.Name)
		}
		seen[string(c.Name)] = true
	}
}

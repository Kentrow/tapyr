package config

import (
	"fmt"
	"slices"
	"strings"
)

// didYouMean returns " (did you mean "x"?)" when one of the candidates is
// close enough to name to be what was meant, and "" otherwise: the
// messages of unknown references and fields end with it.
func didYouMean(name string, candidates []string) string {
	if c := closest(name, candidates); c != "" {
		return fmt.Sprintf(" (did you mean %q?)", c)
	}
	return ""
}

// closest returns the candidate most likely meant by name: the same name
// in another case, else one within a few typos (an edit distance of a
// third of the length, for names of 4 characters or more), else one that
// contains name or that name extends (meraki for meraki-dashboard, key for
// apiKey), the fewest edits first. It returns "" when none is close.
func closest(name string, candidates []string) string {
	n := strings.ToLower(name)
	best, bestDistance := "", 0
	for _, c := range slices.Sorted(slices.Values(candidates)) {
		l := strings.ToLower(c)
		if l == n {
			return c
		}
		d := editDistance(n, l)
		typo := len(n) >= 4 && d <= max(1, min(len(n), len(l))/3)
		part := len(n) >= 3 && (strings.Contains(l, n) || len(l) >= 3 && strings.HasPrefix(n, l))
		if !typo && !part {
			continue
		}
		if best == "" || d < bestDistance {
			best, bestDistance = c, d
		}
	}
	return best
}

// editDistance is the optimal string alignment distance of a and b:
// insertions, deletions, substitutions and transpositions of two adjacent
// characters, the usual typos.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev2 := make([]int, len(rb)+1)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(rb)]
}

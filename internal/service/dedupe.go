package service

import (
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// dedupeIndex finds the entity a push row belongs to: by exact normalized
// email or domain, or (fuzz) by the most similar normalized name.
type dedupeIndex struct {
	mode      string
	entity    string
	threshold float64
	byKey     map[string]*pushed
	entries   []*pushed
}

// key is the normalized dedupe key of an entity's core fields ("" if none).
func (d *dedupeIndex) key(core map[string]*string) string {
	get := func(f string) string {
		if v := core[f]; v != nil {
			return *v
		}
		return ""
	}
	switch d.mode {
	case DedupeEmail:
		return NormalizeEmail(get("email"))
	case DedupeDomain:
		return NormalizeDomain(get("domain"))
	default:
		name := get("name")
		if d.entity == "person" {
			name = strings.TrimSpace(get("first_name") + " " + get("last_name"))
		}
		return NormalizeName(name, d.entity == "organization")
	}
}

func (d *dedupeIndex) add(p *pushed) {
	if p.key == "" {
		return
	}
	if _, ok := d.byKey[p.key]; !ok {
		d.byKey[p.key] = p
	}
	d.entries = append(d.entries, p)
}

// find returns the entity whose key equals key or, in fuzz mode, the first
// most similar one at or above the threshold, with its similarity.
func (d *dedupeIndex) find(key string) (*pushed, float64) {
	if p, ok := d.byKey[key]; ok {
		return p, 1
	}
	if d.mode != DedupeFuzz {
		return nil, 0
	}
	var best *pushed
	bestScore := 0.0
	for _, p := range d.entries {
		if s := Similarity(key, p.key); s >= d.threshold && s > bestScore {
			best, bestScore = p, s
		}
	}
	return best, bestScore
}

// NormalizeEmail lowercases and trims an email address.
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// NormalizeDomain reduces a domain, URL or email address to its lowercase
// host without "www.": "https://www.Acme.io/about" and "ana@acme.io" → "acme.io".
func NormalizeDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if _, rest, ok := strings.Cut(s, "://"); ok {
		s = rest
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "www.")
	return strings.TrimSuffix(s, ".")
}

// legalSuffixes are company-form words dropped from organization names.
var legalSuffixes = []string{"inc", "incorporated", "llc", "ltd", "ltda", "limited", "corp", "corporation",
	"co", "company", "gmbh", "plc", "sa", "ag", "me", "eireli", "the"}

// NormalizeName lowercases a name, strips accents and punctuation and, for
// organizations, company-form words ("Acme, Inc." → "acme").
func NormalizeName(s string, org bool) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	if org {
		kept := slices.DeleteFunc(slices.Clone(words), func(w string) bool { return slices.Contains(legalSuffixes, w) })
		if n := len(kept); n >= 2 && kept[n-2] == "s" && kept[n-1] == "a" {
			kept = kept[:n-2]
		}
		if len(kept) > 0 {
			words = kept
		}
	}
	return strings.Join(words, " ")
}

// Similarity is 1 minus the Levenshtein distance of a and b over the length
// of the longer one (1 = equal, 0 = nothing in common).
func Similarity(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	longest := max(len(ra), len(rb))
	if longest == 0 {
		return 1
	}
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
		}
		prev, cur = cur, prev
	}
	return 1 - float64(prev[len(rb)])/float64(longest)
}

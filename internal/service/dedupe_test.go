package service_test

import (
	"testing"

	"github.com/jdanielnd/crm-cli/internal/service"
	"github.com/stretchr/testify/assert"
)

func TestNormalizeDomain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"acme.io", "acme.io"},
		{" https://www.Acme.io/about?x=1 ", "acme.io"},
		{"http://acme.io:8080", "acme.io"},
		{"Ana@Acme.IO", "acme.io"},
		{"www.acme.io.", "acme.io"},
		{"", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, service.NormalizeDomain(tc.in), tc.in)
	}
}

func TestNormalizeName(t *testing.T) {
	cases := []struct {
		in   string
		org  bool
		want string
	}{
		{"  João  da Silva ", false, "joao da silva"},
		{"O'Brien-Smith", false, "o brien smith"},
		{"ACME Analytics, Inc.", true, "acme analytics"},
		{"Acmé Analytics Ltda", true, "acme analytics"},
		{"Natura S.A.", true, "natura"},
		{"The Company", true, "the company"},
		{"Inc", false, "inc"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, service.NormalizeName(tc.in, tc.org), tc.in)
	}
}

func TestSimilarity(t *testing.T) {
	assert.InDelta(t, 1, service.Similarity("acme", "acme"), 1e-9)
	assert.InDelta(t, 1, service.Similarity("", ""), 1e-9)
	assert.InDelta(t, 0, service.Similarity("abc", "xyz"), 1e-9)
	assert.InDelta(t, 1-1.0/17, service.Similarity("umbrella securty", "umbrella security"), 1e-9)
	assert.InDelta(t, 0.75, service.Similarity("joão", "joao"), 1e-9, "runes, not bytes")
	assert.Less(t, service.Similarity("globex", "initech cloud"), service.DefaultFuzzThreshold)
}

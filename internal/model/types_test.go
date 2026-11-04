package model

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestValidLocator(t *testing.T) {
	long := "omp:x:" + strings.Repeat("s", MaxLocatorBytes-len("omp:x:"))
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"three segments", "omp:~/Sites/resume-builder:w3p2", true},
		{"four segments (bora pane id has a colon)", "omp:~/Sites/x:w19V:p6", true},
		{"empty", "", false},
		{"two segments", "omp:session", false},
		{"empty first segment", ":where:session", false},
		{"empty middle segment", "omp::session", false},
		{"empty last segment", "omp:where:", false},
		{"exactly max bytes", long, true},
		{"one byte over max", long + "s", false},
		{"tab", "omp:wh\tere:s", false},
		{"newline", "omp:where:s\n", false},
		{"DEL", "omp:where:s\x7f", false},
		{"invalid UTF-8 byte", "omp:where:s\xff", false},
		{"non-ASCII UTF-8", "omp:~/Sites/ação:p6", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ValidLocator(tt.in))
		})
	}
}

func TestFormatIdle(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{-time.Minute, "0m"},
		{59 * time.Second, "0m"},
		{time.Minute, "1m"},
		{41*time.Minute + 59*time.Second, "41m"},
		{2 * time.Hour, "2h0m"},
		{2*time.Hour + 41*time.Minute + 30*time.Second, "2h41m"},
		{26 * time.Hour, "26h0m"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, FormatIdle(tt.in))
		})
	}
}

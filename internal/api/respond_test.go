package api

import "testing"

func TestETagMatches(t *testing.T) {
	const etag = `"abc"`
	tests := []struct {
		name    string
		headers []string
		want    bool
	}{
		{name: "no header", headers: nil, want: false},
		{name: "exact", headers: []string{`"abc"`}, want: true},
		{name: "weak form matches (weak comparison)", headers: []string{`W/"abc"`}, want: true},
		{name: "wildcard", headers: []string{"*"}, want: true},
		{name: "list with spaces", headers: []string{`"x" , "abc"`}, want: true},
		{name: "repeated header lines", headers: []string{`"x"`, `"abc"`}, want: true},
		{name: "different", headers: []string{`"abd"`}, want: false},
		{name: "unquoted is not a match", headers: []string{`abc`}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := etagMatches(tt.headers, etag); got != tt.want {
				t.Errorf("etagMatches(%q) = %v, want %v", tt.headers, got, tt.want)
			}
		})
	}
}

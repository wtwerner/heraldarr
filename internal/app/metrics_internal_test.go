package app

import (
	"strings"
	"testing"
)

// Label values are escaped, so any name keeps the exposition parseable.
func TestLabelEscaping(t *testing.T) {
	f := newFamily("x_total", "counter", "Test.", "source")
	f.add(1, "a\"b\\c\nd")
	var b strings.Builder
	f.write(&b)
	if want := `x_total{source="a\"b\\c\nd"} 1`; !strings.Contains(b.String(), want+"\n") {
		t.Errorf("got\n%s\nwant a line %s", b.String(), want)
	}
}

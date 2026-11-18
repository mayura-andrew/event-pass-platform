package httphandler

import "testing"

// Every page template parses (the integration tests render them, but only
// with a database).
func TestPagesParse(t *testing.T) {
	if _, err := loadPages(); err != nil {
		t.Fatal(err)
	}
}

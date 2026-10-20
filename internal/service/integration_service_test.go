package service

import "testing"

func TestFormID(t *testing.T) {
	const id = "1AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-xy"
	for _, in := range []string{id, "https://docs.google.com/forms/d/" + id + "/edit",
		"https://docs.google.com/forms/d/" + id + "/edit#responses", " https://docs.google.com/forms/d/" + id + " "} {
		if got, err := FormID(in); err != nil || got != id {
			t.Errorf("FormID(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "hello", "https://docs.google.com/forms/d/e/1FAIpQLSd" + id + "/viewform",
		"https://example.com/forms/d/short/edit"} {
		if got, err := FormID(in); err == nil {
			t.Errorf("FormID(%q) = %q, want an error", in, got)
		}
	}
}

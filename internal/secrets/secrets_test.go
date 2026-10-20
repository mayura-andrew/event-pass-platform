package secrets

import (
	"bytes"
	"testing"
)

func TestSealOpen(t *testing.T) {
	b := Derived([]byte("x"))
	sealed, err := b.Seal([]byte("refresh-token"), []byte("row-1"))
	if err != nil || bytes.Contains(sealed, []byte("refresh-token")) {
		t.Fatalf("sealed %x, %v", sealed, err)
	}
	if got, err := b.Open(sealed, []byte("row-1")); err != nil || string(got) != "refresh-token" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := b.Open(sealed, []byte("row-2")); err == nil {
		t.Error("opened with another row's associated data")
	}
	if _, err := Derived([]byte("y")).Open(sealed, []byte("row-1")); err == nil {
		t.Error("opened with another key")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Error("short key accepted")
	}
}

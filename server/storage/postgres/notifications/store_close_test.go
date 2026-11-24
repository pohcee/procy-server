package notifications

import "testing"

func TestCloseTwice(t *testing.T) {
	s := New(nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

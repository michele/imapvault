package mail

import "testing"

func TestCapabilitiesHas(t *testing.T) {
	c := Capabilities{"CONDSTORE", "UIDPLUS"}
	if !c.Has("condstore") || c.Has("QRESYNC") {
		t.Fatal("Has must be case-insensitive and exact")
	}
	if c.String() != "CONDSTORE UIDPLUS" {
		t.Fatalf("String = %q", c.String())
	}
}

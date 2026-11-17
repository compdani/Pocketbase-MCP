package pocketbasemcp

import "testing"

func TestAcceptPrincipalFailsClosed(t *testing.T) {
	s := &Server{cfg: Config{}}
	if s.acceptPrincipal(Principal{ID: "u1", CollectionName: "users"}) {
		t.Fatal("empty Auth.Collections must reject")
	}
	s.cfg.Auth.Collections = []string{"users"}
	if !s.acceptPrincipal(Principal{ID: "u1", CollectionName: "users"}) {
		t.Fatal("listed collection must be accepted")
	}
	if s.acceptPrincipal(Principal{ID: "s1", CollectionName: "_superusers", IsSuperuser: true}) {
		t.Fatal("superusers must be rejected by default")
	}
}

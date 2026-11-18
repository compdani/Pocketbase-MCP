package chat

import "testing"

func ownerFixture() OwnerKey {
	return OwnerKey{
		Namespace:        "app",
		Mount:            "/mcp",
		AuthCollectionID: "users0000000001",
		UserID:           "user00000000001",
	}
}

func TestOwnerKeyValidate(t *testing.T) {
	if err := ownerFixture().Validate(); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	full := ownerFixture()
	full.Tenant = "tenant"
	if err := full.Validate(); err != nil {
		t.Fatalf("valid key with tenant rejected: %v", err)
	}
	cases := map[string]func(*OwnerKey){
		"namespace":        func(k *OwnerKey) { k.Namespace = "" },
		"mount":            func(k *OwnerKey) { k.Mount = "" },
		"authCollectionId": func(k *OwnerKey) { k.AuthCollectionID = "" },
		"userId":           func(k *OwnerKey) { k.UserID = "" },
	}
	for name, mutate := range cases {
		k := ownerFixture()
		mutate(&k)
		if err := k.Validate(); err == nil {
			t.Errorf("missing %s accepted", name)
		}
	}
}

func TestOwnerKeyStringIsUnambiguous(t *testing.T) {
	a := OwnerKey{Namespace: "a|b", Mount: "c", AuthCollectionID: "d", UserID: "e"}
	b := OwnerKey{Namespace: "a", Mount: "b|c", AuthCollectionID: "d", UserID: "e"}
	if a.String() == b.String() {
		t.Fatalf("owner keys collided: %q", a.String())
	}

	// Field-count ambiguity: an empty tenant must not look like a shifted field.
	c := OwnerKey{Namespace: "a", Mount: "b", AuthCollectionID: "c", UserID: "d"}
	d := OwnerKey{Namespace: "a", Mount: "b|c", AuthCollectionID: "d", UserID: ""}
	if c.String() == d.String() {
		t.Fatalf("owner keys collided: %q", c.String())
	}
	if c.Equal(d) {
		t.Fatal("different keys reported equal")
	}

	// A tenant is part of identity.
	e := c
	e.Tenant = "t1"
	f := c
	f.Tenant = "t2"
	if e.String() == f.String() || e.Equal(f) {
		t.Fatal("tenant must participate in the key")
	}

	// Same key is stable and equal.
	if c.String() != c.String() {
		t.Fatal("owner key encoding is not stable")
	}
	if !c.Equal(ownerKeyCopy(c)) {
		t.Fatal("identical keys must be equal")
	}
}

func TestOwnerKeyStringDistinguishesAllFields(t *testing.T) {
	base := ownerFixture()
	seen := map[string]OwnerKey{base.String(): base}
	mutations := []func(*OwnerKey){
		func(k *OwnerKey) { k.Namespace = "other" },
		func(k *OwnerKey) { k.Mount = "/other" },
		func(k *OwnerKey) { k.AuthCollectionID = "other" },
		func(k *OwnerKey) { k.UserID = "other" },
		func(k *OwnerKey) { k.Tenant = "other" },
	}
	for i, mutate := range mutations {
		k := base
		mutate(&k)
		s := k.String()
		if prev, ok := seen[s]; ok {
			t.Fatalf("mutation %d collides with %s: %q", i, prev.String(), s)
		}
		seen[s] = k
	}
}

func ownerKeyCopy(k OwnerKey) OwnerKey { return k }

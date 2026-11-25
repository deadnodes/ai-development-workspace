package imagesigning

import "testing"

func TestRepositoryIdentityRemovesTagAndDigest(t *testing.T) {
	got, ok := RepositoryIdentity("registry.deadnodes.internal/deadnodes/pp-back/runtime:main-758b7130@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if !ok || got != "registry.deadnodes.internal/deadnodes/pp-back/runtime" {
		t.Fatalf("identity=%q ok=%v", got, ok)
	}
}

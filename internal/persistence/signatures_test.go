package persistence

import (
	"context"
	"path/filepath"
	"reflect"
	"releasecontrol/internal/domain"
	"testing"
	"time"
)

func TestSignaturePersistenceLocal(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "state.db")
	s, e := OpenSQLite(ctx, file)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	entry := domain.BuildSignature{ImageID: "image", Digest: "sha256:digest", KeyID: "dev", KeyVersion: 2, KeyFingerprint: "fingerprint", SignatureTag: "sha256-digest.sig", Attestations: []string{"https://slsa.dev/provenance/v1"}, Status: "SIGNED", SignedAt: &now}
	want := domain.ArtifactSignature{Meta: domain.Meta{ID: "sig", ProductID: "p", CreatedAt: now}, Request: domain.SignArtifactRequest{ProductID: "p", RequestKey: "request"}, Status: "SIGNED", Signatures: []domain.BuildSignature{entry}}
	if e = s.Update(ctx, func(st *domain.State) error { st.ArtifactSignatures = append(st.ArtifactSignatures, want); return nil }); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = OpenSQLite(ctx, file)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	st, e := s.Read(ctx)
	if e != nil || len(st.ArtifactSignatures) != 1 || !reflect.DeepEqual(st.ArtifactSignatures[0], want) {
		t.Fatal(st.ArtifactSignatures, e)
	}
}

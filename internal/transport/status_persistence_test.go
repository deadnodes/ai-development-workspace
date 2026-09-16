package transport_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
)

func TestPostgresRejectsUnknownStatusThroughStoreAndBackup(t *testing.T) {
	ctx := context.Background()
	source, err := persistence.Open(ctx, isolatedDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	service := application.New(source)
	for _, c := range []domain.Command{{Action: "create_product", ID: "product", Data: map[string]any{"name": "Product"}}, {Action: "create_feature", ID: "feature", ProductID: "product", Data: map[string]any{"title": "Feature", "goal": "Strict lifecycle"}}} {
		c.Actor = "agent"
		if _, err = service.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	before, err := source.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Update(ctx, func(st *domain.State) error { st.Features[0].Status = "anything-goes"; return nil }); err == nil || !strings.Contains(err.Error(), "feature") {
		t.Fatalf("direct store bypass accepted: %v", err)
	}
	after, err := source.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected store write changed persisted state")
	}
	// Recompute a valid checksum around invalid lifecycle data: checksum integrity
	// cannot substitute for semantic validation on the restore path.
	corrupt := before
	corrupt.Features = append([]domain.Feature{}, before.Features...)
	corrupt.Features[0].Status = "anything-goes"
	data, err := json.Marshal(map[string]any{"format": "release-control-backup", "version": 1, "state": corrupt})
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err = writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(compressed.Bytes())
	dest, err := persistence.Open(ctx, isolatedDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()
	if _, err = application.New(dest).RestoreBackup(ctx, "agent", base64.StdEncoding.EncodeToString(compressed.Bytes()), hex.EncodeToString(hash[:])); err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("backup bypass accepted: %v", err)
	}
	restored, err := dest.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, domain.EmptyState()) {
		t.Fatal("rejected backup partially restored")
	}
}

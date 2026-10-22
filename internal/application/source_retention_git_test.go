package application

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	githubprovider "releasecontrol/internal/providers/github"
)

func sourceRetentionGitFixture(t *testing.T) (*Service, *memoryStore, *featurePatchGitProvider, *featurePatchGitAPI, string) {
	t.Helper()
	f := &featurePatchGitAPI{t: t, dir: t.TempDir()}
	f.mustGit("init", "--quiet", "-b", "main")
	f.mustGit("config", "user.email", "fixture@example.test")
	f.mustGit("config", "user.name", "Source retention fixture")
	commit := func(file, text string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(f.dir, file), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		f.mustGit("add", file)
		f.mustGit("commit", "--quiet", "-m", text)
		return f.mustGit("rev-parse", "HEAD")
	}
	base := commit("base.txt", "base")
	f.mustGit("checkout", "--quiet", "-b", "feature/a")
	a := commit("a.txt", "A")
	f.mustGit("checkout", "--quiet", "-b", "feature/b", base)
	b := commit("b.txt", "B")
	f.mustGit("checkout", "--quiet", "main")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RCP_RETENTION_GIT_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	t.Setenv("RCP_GITHUB_API_URL", server.URL)
	s, m, fake := flowFixture(t)
	fake.main = base
	provider := &featurePatchGitProvider{fakeFlow: fake, github: githubprovider.NewWithClient(server.Client()), conn: delivery.Connection{AppID: 1, InstallationID: 2, Owner: "owner", APIURL: server.URL, PrivateKeyRef: "env:RCP_RETENTION_GIT_KEY"}}
	s.provider = provider
	exec(t, s, domain.Command{Action: "create_feature", ID: "fb", ProductID: "p", Data: map[string]any{"title": "B", "goal": "B"}})
	exec(t, s, domain.Command{Action: "create_integration", ID: "ib", FeatureID: "fb", Data: map[string]any{"title": "B", "objective": "B", "repositories": []string{"source"}}})
	for _, r := range []struct{ id, in, head string }{{"a1", "i", a}, {"b1", "ib", b}, {"alias-a", "i", a}} {
		exec(t, s, domain.Command{Action: "record_integration_revision", ID: r.id, IntegrationID: r.in, Data: map[string]any{"repository_id": "source", "branch": "feature/" + r.in, "base_commit": base, "head_commit": r.head, "commits": []string{r.head}}})
	}
	exec(t, s, domain.Command{Action: "plan_composition", ID: "retained-git", ProductID: "p", Data: map[string]any{"name": "A+B", "environment_id": "dev", "components": []domain.CompositionComponent{{ApplicationID: "component", BaseRef: "main", BaseCommit: base, TargetBranch: "generated/retained", RevisionIDs: []string{"a1", "b1"}}}}})
	parent := exec(t, s, domain.Command{Action: "reconcile_composition", ID: "retained-git"}).(domain.ExternalOperation)
	return s, m, provider, f, parent.ID
}

func durableRetentionTick(t *testing.T, s *Service, store workerTestStore) {
	t.Helper()
	if err := store.Update(context.Background(), func(st *domain.State) error {
		for i := range st.Operations {
			st.Operations[i].NextAttemptAt = time.Time{}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSourceRetentionRealGitDurableReopenAndBranchDeletion(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			_, memory, provider, git, parent := sourceRetentionGitFixture(t)
			store, reopen := openWorkerTestStore(t, kind)
			defer func() { store.Close() }()
			if err := store.Update(ctx, func(st *domain.State) error { *st = memory.state; return nil }); err != nil {
				t.Fatal(err)
			}
			s := NewWithProvider(store, provider)
			// The remote exact ref is created, its response lost, then the same lease
			// expires before local admission. Reopening must observe rather than rewrite.
			git.loseRetentionCreate = true
			provider.afterEnsure = func() {
				if err := store.Update(ctx, func(st *domain.State) error {
					operationByID(st, parent).LeaseUntil = time.Now().Add(-time.Second)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Tick(ctx); err == nil {
				t.Fatal("expired same-owner lease admitted remote pin")
			}
			before, _ := store.Read(ctx)
			if len(operationByID(&before, parent).Sources[0].Retained) != 0 {
				t.Fatal("late pin admitted")
			}
			refSHA := git.mustGit("rev-parse", "refs/heads/rcp/revisions/a1")
			if refSHA != revisionByID(&before, "a1").HeadCommit {
				t.Fatal("lost create did not leave exact retained object")
			}
			writes := len(git.writes)
			store.Close()
			store = reopen()
			s = NewWithProvider(store, provider)
			durableRetentionTick(t, s, store)
			after, _ := store.Read(ctx)
			if len(operationByID(&after, parent).Sources[0].Retained) != 1 || len(git.writes) != writes {
				t.Fatal("reopen rewrote existing retained ref or lost progress")
			}
			for range 8 {
				durableRetentionTick(t, s, store)
				after, _ = store.Read(ctx)
				if retentionSourceReady(*operationByID(&after, parent)) {
					break
				}
			}
			source := operationByID(&after, parent).Sources[0]
			if source.Result == nil || len(source.Retained) != 2 {
				t.Fatal("missing exact retained generated source")
			}
			if git.mustGit("show", source.Result.SHA+":a.txt") != "A" || git.mustGit("show", source.Result.SHA+":b.txt") != "B" {
				t.Fatal("retained Git tree dropped A or B")
			}
			for _, rev := range source.Retention.Revisions {
				if git.mustGit("rev-parse", "refs/heads/"+rev.Branch) != rev.HeadCommit {
					t.Fatal("retained ref identity mismatch")
				}
			}
			git.mustGit("update-ref", "-d", "refs/heads/feature/a")
			git.mustGit("update-ref", "-d", "refs/heads/feature/b")
			// Both saved-result consumption and the actual child-queue boundary reread
			// retained refs and structural generated provenance after developer deletion.
			for range 4 {
				durableRetentionTick(t, s, store)
				after, _ = store.Read(ctx)
				if len(operationByID(&after, parent).ChildIDs) > 0 {
					break
				}
			}
			current := operationByID(&after, parent)
			if len(current.ChildIDs) != 1 || current.Sources[0].Result.SHA != source.Result.SHA {
				t.Fatalf("retained inputs unavailable after branch deletion: %+v", current)
			}
			child := operationByID(&after, current.ChildIDs[0])
			if child.Snapshot.BuildRequest.SourceSHA != source.Result.SHA {
				t.Fatal("child did not pin verified generated SHA")
			}
			if len(provider.builds) != 0 || provider.advances != 0 {
				t.Fatal("source fixture performed build/main writes")
			}
			// A different immutable revision ID at the same SHA gets its own ref in a
			// separate operation; earlier snapshots/proofs remain unchanged.
			frozen := cloneFlowOperation(*current)
			comp := *compositionByID(&after, "retained-git")
			comp.Components[0].RevisionIDs = []string{"alias-a"}
			exec(t, s, domain.Command{Action: "plan_composition", ID: "alias-plan", ProductID: "p", Data: map[string]any{"name": "same SHA new revision", "environment_id": "dev", "components": comp.Components}})
			alias := exec(t, s, domain.Command{Action: "reconcile_composition", ID: "alias-plan"}).(domain.ExternalOperation)
			// Stop old component work; source test never executes build/provider delivery.
			_ = store.Update(ctx, func(st *domain.State) error {
				for i := range st.Operations {
					if st.Operations[i].ID != alias.ID {
						st.Operations[i].NextAttemptAt = time.Now().Add(time.Hour)
					}
				}
				return nil
			})
			if _, err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if git.mustGit("rev-parse", "refs/heads/rcp/revisions/alias-a") != refSHA {
				t.Fatal("same SHA revision alias lost exact independent ref")
			}
			after, _ = store.Read(ctx)
			old := operationByID(&after, parent)
			if !reflect.DeepEqual(old.Sources, frozen.Sources) || !reflect.DeepEqual(old.CompositionSnapshot, frozen.CompositionSnapshot) {
				t.Fatal("new pin rewrote prior immutable snapshots/proof")
			}
			backup, err := s.CreateBackup(ctx, "tester")
			if err != nil {
				t.Fatal(err)
			}
			restored, _ := openWorkerTestStore(t, kind)
			defer restored.Close()
			rs := NewWithProvider(restored, provider)
			if _, err = rs.RestoreBackup(ctx, "tester", backup.ArchiveBase64, backup.SHA256); err != nil {
				t.Fatal(err)
			}
			restoredState, _ := restored.Read(ctx)
			restoredOp := operationByID(&restoredState, alias.ID)
			if restoredOp.Status != "CANCELLED" || !reflect.DeepEqual(restoredOp.Sources, operationByID(&after, alias.ID).Sources) {
				t.Fatal("restore bypassed quarantine or rewrote retained history")
			}
			t.Logf("%s actual Git: lost POST + expired same-owner lease + reopen, A/B refs survive developer deletion, exact child source=%s, same-SHA separate revision ref, restore quarantine", kind, source.Result.SHA)
		})
	}
}

func TestSourceRetentionRealGitStageDriftBlocksSavedResult(t *testing.T) {
	for _, stage := range []string{"final", "step"} {
		t.Run(stage, func(t *testing.T) {
			s, m, provider, git, parent := sourceRetentionGitFixture(t)
			flowTickUntil(t, s, m, parent, retentionSourceReady)
			source := operationByID(&m.state, parent).Sources[0]
			branch := source.Plan.TargetBranch
			if stage == "step" {
				branch += "-step-1"
			}
			git.mustGit("update-ref", "refs/heads/"+branch, source.Plan.BaseSHA)
			op, token := retentionClaim(t, s, parent)
			if err := s.tickFlow(context.Background(), op, token); err != nil {
				t.Fatal(err)
			}
			current := operationByID(&m.state, parent)
			if current.Status != "FAILED" || len(current.ChildIDs) != 0 || len(provider.builds) != 0 || !strings.Contains(current.Detail, "generated source drift") {
				t.Fatalf("drifted %s source consumed: %+v", stage, current)
			}
		})
	}
}

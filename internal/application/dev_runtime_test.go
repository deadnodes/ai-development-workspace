package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

type devRuntimeProvider struct {
	activationHeads    map[string]string
	activationReadHead string
	unprovableEndpoint string
	*devActivationProvider
	runtime                                 bool
	head                                    string
	images                                  map[string]string
	digests                                 map[string]string
	reads, revisions                        int
	onRead, onRevision                      func()
	failRead, failRevision, foreignAncestry bool
	blob                                    string
}

func (p *devRuntimeProvider) ProposeGitOpsGuarded(ctx context.Context, c delivery.Connection, r delivery.GitOpsApply, g func(context.Context) error) (delivery.GitOpsPullRequest, error) {
	p.images[r.Request.Path] = r.Request.ImageRepository
	p.digests[r.Request.Path] = r.Digest
	return p.devActivationProvider.ProposeGitOpsGuarded(ctx, c, r, g)
}
func (p *devRuntimeProvider) ObserveGitOpsPR(ctx context.Context, c delivery.Connection, r delivery.GitOpsApply, pr delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	out, err := p.devActivationProvider.ObserveGitOpsPR(ctx, c, r, pr)
	if out.Merged {
		p.activationReadHead = p.activationHeads[r.OperationID]
	}
	return out, err
}
func (p *devRuntimeProvider) ReadGitOps(ctx context.Context, c delivery.Connection, r delivery.GitOpsRequest) (delivery.GitOpsSnapshot, error) {
	if !p.merged {
		return p.devActivationProvider.ReadGitOps(ctx, c, r)
	}
	head := p.activationReadHead
	if head == "" {
		head = strings.Repeat("f", 40)
	}
	if p.runtime {
		head = p.head

	}
	if p.runtime {
		p.reads++
		if p.onRead != nil {
			p.onRead()
		}
		if p.failRead {
			return delivery.GitOpsSnapshot{}, errors.New("secret rawproviderpayload must not persist")
		}
	}
	if r.ExpectedHeadSHA != "" && head != r.ExpectedHeadSHA {
		return delivery.GitOpsSnapshot{}, errors.New("head changed")
	}
	blob := p.blob
	if blob == "" {
		blob = strings.Repeat("d", 40)
	}
	return delivery.GitOpsSnapshot{HeadSHA: head, BlobSHA: blob, ImageRepository: p.images[r.Path], Digest: p.digests[r.Path], Content: "shared-file-with-A-and-B-at-current-head"}, nil
}
func (p *devRuntimeProvider) VerifyGitOpsRevision(_ context.Context, connection delivery.Connection, r delivery.GitOpsRevisionRequest) (delivery.GitOpsRevisionProof, error) {
	p.revisions++
	if p.onRevision != nil {
		p.onRevision()
	}
	if p.unprovableEndpoint != "" && connection.APIURL == p.unprovableEndpoint {
		return delivery.GitOpsRevisionProof{}, errors.New("foreign authority ancestry unavailable")
	}
	if p.failRevision || r.HeadSHA != "" && r.HeadSHA != p.head {
		return delivery.GitOpsRevisionProof{}, errors.New("unprovable ancestry")
	}
	ancestors := append([]string(nil), r.AncestorSHAs...)
	if p.foreignAncestry {
		ancestors = nil
	}
	return delivery.GitOpsRevisionProof{Repository: r.Repository, Ref: r.Ref, HeadSHA: p.head, AncestorSHAs: ancestors}, nil
}

type devRuntimeObserver struct {
	scope      delivery.RuntimeVerificationScope
	value      delivery.RuntimeSnapshot
	onSnapshot func()
	scopeError bool
	calls      int
}

func (o *devRuntimeObserver) TargetIdentity() delivery.RuntimeTarget {
	return delivery.RuntimeTarget{ProductID: o.scope.ProductID, EnvironmentID: o.scope.EnvironmentID, ApplicationID: o.scope.ApplicationID}
}
func (o *devRuntimeObserver) RuntimeVerificationScope() (delivery.RuntimeVerificationScope, error) {
	if o.scopeError {
		return o.scope, errors.New("binding unavailable")
	}
	return o.scope, nil
}
func (o *devRuntimeObserver) Snapshot(context.Context) delivery.RuntimeSnapshot {
	o.calls++
	if o.onSnapshot != nil {
		o.onSnapshot()
	}
	v := o.value
	v.ObservedAt = time.Now().UTC()
	v.Pods = append([]delivery.RuntimePod(nil), v.Pods...)
	return v
}

type runtimeInformationalOnly struct{ value *devRuntimeObserver }

func (o runtimeInformationalOnly) TargetIdentity() delivery.RuntimeTarget {
	return o.value.TargetIdentity()
}
func (o runtimeInformationalOnly) Snapshot(ctx context.Context) delivery.RuntimeSnapshot {
	return o.value.Snapshot(ctx)
}
func runtimeVerifyCommand(activation domain.ExternalOperation, key string) domain.Command {
	return domain.Command{Action: "verify_candidate_dev_runtime", ProductID: activation.ProductID, Actor: activation.RequestedBy, Data: map[string]any{"activation_operation_id": activation.ID, "request_key": key, "expected_selection": activation.DevActivation.Request.ExpectedSelection}}
}
func runtimeQuery(t *testing.T, f *nativeFixture) map[string]any {
	t.Helper()
	v, e := f.s.Query(context.Background(), "get_environment_state", "dev")
	if e != nil {
		t.Fatal(e)
	}
	return v.(map[string]any)
}
func runRuntimeVerification(t *testing.T, f *nativeFixture, c domain.Command) domain.ExternalOperation {
	t.Helper()
	v, e := f.s.Execute(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	op := v.(domain.ExternalOperation)
	for range 3 {
		nativeTick(t, f)
		st, _ := f.store.Read(context.Background())
		op = *operationByID(&st, op.ID)
		if terminalOperation(op.Status) {
			break
		}
	}
	return op
}
func devRuntimeFixture(t *testing.T, kind string, multi bool, endpoint ...string) (*nativeFixture, *devRuntimeProvider, domain.ExternalOperation, []*devRuntimeObserver) {
	t.Helper()
	var f *nativeFixture
	var p *devActivationProvider
	var c domain.Command
	if multi {
		f, p, _, c = devActivationMultiFixture(t, kind, func(f *nativeFixture) {
			// Separate already-authorized connection aliases address one numeric repo.
			// Configure fixture identities before any candidate/reservation proof exists.
			if err := f.store.Update(context.Background(), func(st *domain.State) error {
				conn := *connection(st, "conn")
				conn.ID = "gitops-connection-b"
				conn.Config.ID = conn.ID
				conn.Config.APIURL = "https://api.github.com/"
				if len(endpoint) > 0 {
					conn.Config.APIURL = endpoint[0]
				}
				conn.Config.Owner = "second-alias"
				st.GitHubConnections = append(st.GitHubConnections, conn)
				grant := st.ConnectionGrants[0]
				grant.ID = "runtime-second-grant"
				grant.ConnectionID = conn.ID
				st.ConnectionGrants = append(st.ConnectionGrants, grant)
				binding := *repositoryBinding(st, "gitops")
				binding.ID = "gitops-binding-b"
				binding.RepositoryID = "gitops-physical-b"
				for _, repo := range st.Repositories {
					if repo.ID == "gitops" {
						repo.ID = binding.RepositoryID
						st.Repositories = append(st.Repositories, repo)
						break
					}
				}
				binding.ConnectionID = conn.ID
				st.RepositoryBindings = append(st.RepositoryBindings, binding)
				target := environmentBinding(st, "dev", "component-b")
				target.ConnectionID = conn.ID
				target.RepositoryID = binding.RepositoryID
				target.Path = "components-shared.yaml"
				environmentBinding(st, "dev", "component").Path = target.Path
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	} else {
		f, p, _, c = devActivationFixture(t, kind)
	}
	provider := &devRuntimeProvider{devActivationProvider: p, head: strings.Repeat("c", 40), images: map[string]string{}, digests: map[string]string{}}
	if multi {
		provider.head = strings.Repeat("b", 40)
	}
	f.s.provider = provider
	v, e := f.s.Execute(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	activation := v.(domain.ExternalOperation)
	provider.activationHeads = map[string]string{}
	for i, cid := range activation.ChildIDs {
		head := strings.Repeat("f", 40)
		if i > 0 {
			head = strings.Repeat("b", 40)
		}
		provider.activationHeads[cid] = head
	}
	activationTicks(t, f, 10)
	p.merged = true
	activationTicks(t, f, 16)
	st, _ := f.store.Read(context.Background())
	activation = *operationByID(&st, activation.ID)
	if activation.Status != "SUCCEEDED" {
		t.Fatal("runtime fixture activation", activation.Status, activation.Detail)
	}
	provider.runtime = true
	var observers []*devRuntimeObserver
	var configured []delivery.RuntimeSnapshotObserver
	for _, cid := range activation.ChildIDs {
		child := operationByID(&st, cid)
		for _, role := range []string{"http", "worker"} {
			scope := delivery.RuntimeVerificationScope{ProductID: "p", EnvironmentID: "dev", ApplicationID: child.ApplicationID, Context: "local", Namespace: "dev", Deployment: child.ApplicationID + "-" + role, Container: role, FluxNamespace: "flux-system", Kustomization: "apps-dev", VerificationGroup: "complete-v1", BindingHash: strings.Repeat("a", 64)}
			image := child.ExistingArtifact.ImageRepository + "@" + child.ExpectedDigest
			o := &devRuntimeObserver{scope: scope, value: delivery.RuntimeSnapshot{Context: scope.Context, ProductID: "p", EnvironmentID: "dev", ApplicationID: scope.ApplicationID, Namespace: scope.Namespace, Deployment: scope.Deployment, Container: scope.Container, Generation: 2, ObservedGeneration: 2, DesiredReplicas: 1, Health: "HEALTHY", WorkloadImage: image, Pods: []delivery.RuntimePod{{Name: scope.Deployment + "-pod", UID: scope.Deployment + "-uid", Phase: "Running", Image: image, ImageID: "containerd://" + child.ExpectedDigest, Ready: true}}, Flux: &delivery.RuntimeFluxSnapshot{Status: "READY", Revision: "main@sha1:" + provider.head}}}
			observers = append(observers, o)
			configured = append(configured, o)
		}
	}
	f.s.SetRuntimeObservers(configured)
	return f, provider, activation, observers
}
func cloneRuntimeState(st domain.State) domain.State {
	b, _ := json.Marshal(st)
	var out domain.State
	_ = json.Unmarshal(b, &out)
	return out
}

func TestDevRuntimeExactObservationReplayAndCurrentTruth(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, activation, observers := devRuntimeFixture(t, kind, false)
			before, _ := f.store.Read(ctx)
			c := runtimeVerifyCommand(activation, "runtime-exact-first-0001")
			op := runRuntimeVerification(t, f, c)
			if op.Status != "SUCCEEDED" || op.Phase != "RUNTIME_VERIFIED" || op.DevRuntime == nil || op.DevRuntime.Generation != 1 || op.DevRuntimeResult == nil || len(op.DevRuntimeResult.Components[0].Roles) != 2 || p.revisions != 2 {
				t.Fatal("verification failed", op.Status, op.Detail, op.DevRuntimeResult)
			}
			after, _ := f.store.Read(ctx)
			if !reflect.DeepEqual(before.Environments, after.Environments) || !reflect.DeepEqual(before.ComponentBuilds, after.ComponentBuilds) || !reflect.DeepEqual(before.DeliveryArtifacts, after.DeliveryArtifacts) || !reflect.DeepEqual(before.RuntimeObservations, after.RuntimeObservations) || !reflect.DeepEqual(before.RuntimeSnapshots, after.RuntimeSnapshots) || !reflect.DeepEqual(*operationByID(&before, activation.ID), *operationByID(&after, activation.ID)) {
				t.Fatal("runtime verification rewrote baseline or provenance")
			}
			for _, cid := range activation.ChildIDs {
				if !reflect.DeepEqual(*operationByID(&before, cid), *operationByID(&after, cid)) {
					t.Fatal("runtime verification rewrote activated child")
				}
			}
			q := runtimeQuery(t, f)
			if q["candidate_runtime_verified"] != true || q["candidate_runtime_verification_id"] != op.ID {
				t.Fatal("missing separate verified query", q["candidate_runtime_verified"])
			}
			reads, events := p.reads, len(after.Events)
			v, e := f.s.Execute(ctx, c)
			if e != nil || v.(domain.ExternalOperation).ID != op.ID {
				t.Fatal("historical replay", e)
			}
			replayed, _ := f.store.Read(ctx)
			if p.reads != reads || len(replayed.Events) != events {
				t.Fatal("replay performed IO/audit")
			}
			if _, e = f.s.Execute(ctx, releaseReservationCommand(after, *activeDevReservation(&after, "dev"))); e == nil {
				t.Fatal("runtime proof unlocked hold")
			}
			// Reopen preserves evidence but never depends on wall-clock ordering of requests.
			f.store.Close()
			f.store = f.reopen()
			f.s.store = f.store
			if runtimeQuery(t, f)["candidate_runtime_verified"] != true {
				t.Fatal("reopen lost runtime proof")
			}
			for _, mutate := range []func(*delivery.RuntimeVerificationScope){func(s *delivery.RuntimeVerificationScope) { s.BindingHash = strings.Repeat("b", 64) }, func(s *delivery.RuntimeVerificationScope) { s.VerificationGroup = "replacement" }, func(s *delivery.RuntimeVerificationScope) { s.Deployment = "replacement" }} {
				saved := observers[0].scope
				mutate(&observers[0].scope)
				if runtimeQuery(t, f)["candidate_runtime_verified"] != false {
					t.Fatal("coverage/binding rebind retained current truth")
				}
				observers[0].scope = saved
			}
			configured := append([]delivery.RuntimeSnapshotObserver(nil), f.s.runtimeObservers...)
			f.s.SetRuntimeObservers(configured[:1])
			if runtimeQuery(t, f)["candidate_runtime_verified"] != false {
				t.Fatal("removed role retained current truth")
			}
			f.s.SetRuntimeObservers(configured)
			extra := *observers[0]
			extra.scope.Deployment = "extra-role"
			f.s.SetRuntimeObservers(append(append([]delivery.RuntimeSnapshotObserver(nil), configured...), &extra))
			if runtimeQuery(t, f)["candidate_runtime_verified"] != false {
				t.Fatal("added role retained current truth")
			}
			f.s.SetRuntimeObservers(configured)
			// A later old-clock gate FAIL invalidates current truth, even with unchanged runtime.
			fail := before.Results[len(before.Results)-1]
			fail.ID = "late-old-clock-runtime-fail"
			fail.Result = "failed"
			fail.CreatedAt = time.Unix(1, 0)
			if e = f.store.Update(ctx, func(st *domain.State) error { st.Results = append(st.Results, fail); return nil }); e != nil {
				t.Fatal(e)
			}
			if runtimeQuery(t, f)["candidate_runtime_verified"] != false {
				t.Fatal("later FAIL retained current runtime truth")
			}
			reads = p.reads
			if v, e = f.s.Execute(ctx, c); e != nil || v.(domain.ExternalOperation).ID != op.ID || p.reads != reads {
				t.Fatal("invalidated historical replay minted rights", e)
			}
		})
	}
}

func TestDevRuntimeAdmissionCoverageAndImmutableGuards(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, _, activation, observers := devRuntimeFixture(t, kind, false)
			ctx := context.Background()
			command := runtimeVerifyCommand(activation, "runtime-guard-request-0001")
			for name, mutate := range map[string]func(*domain.Command){"foreign-product": func(c *domain.Command) { c.ProductID = "foreign" }, "foreign-owner": func(c *domain.Command) { c.Actor = "other" }, "unknown": func(c *domain.Command) { c.Data["healthy"] = true }, "case": func(c *domain.Command) { c.Data["Request_Key"] = "forged" }, "authority": func(c *domain.Command) { c.ID = "caller-id" }, "cas": func(c *domain.Command) { c.Data["expected_selection"] = domain.EnvironmentSelection{} }, "short-key": func(c *domain.Command) { c.Data["request_key"] = "short" }} {
				t.Run(name, func(t *testing.T) {
					c := command
					c.Data = map[string]any{}
					for k, v := range command.Data {
						c.Data[k] = v
					}
					mutate(&c)
					if _, e := f.s.Execute(ctx, c); e == nil {
						t.Fatal("admitted invalid runtime request")
					}
				})
			}
			original := append([]delivery.RuntimeSnapshotObserver(nil), f.s.runtimeObservers...)
			for _, bad := range [][]delivery.RuntimeSnapshotObserver{nil, {runtimeInformationalOnly{observers[0]}, observers[1]}, {observers[0], observers[0]}} {
				f.s.SetRuntimeObservers(bad)
				if _, e := f.s.Execute(ctx, command); e == nil {
					t.Fatal("admitted missing/informational/duplicate coverage")
				}
			}
			f.s.SetRuntimeObservers(original)
			for _, field := range []string{"group", "mixed", "context", "namespace", "binding", "error"} {
				saved := observers[0].scope
				switch field {
				case "group":
					observers[0].scope.VerificationGroup = ""
				case "mixed":
					observers[0].scope.VerificationGroup = "mixed"
				case "context":
					observers[0].scope.Context = "wrong"
				case "namespace":
					observers[0].scope.Namespace = "wrong"
				case "binding":
					observers[0].scope.BindingHash = ""
				case "error":
					observers[0].scopeError = true
				}
				if _, e := f.s.Execute(ctx, command); e == nil {
					t.Fatal("admitted invalid coverage", field)
				}
				observers[0].scope = saved
				observers[0].scopeError = false
			}
			v, e := f.s.Execute(ctx, command)
			if e != nil {
				t.Fatal(e)
			}
			op := v.(domain.ExternalOperation)
			state, _ := f.store.Read(ctx)
			for name, mutate := range map[string]func(*domain.State){"epoch": func(st *domain.State) { environmentByID(st, "dev").ReservationEpoch++ }, "selection": func(st *domain.State) { environmentByID(st, "dev").SelectionGeneration++ }, "artifact": func(st *domain.State) { st.DeliveryArtifacts[0].Digest = "sha256:" + strings.Repeat("f", 64) }, "child-result": func(st *domain.State) {
				operationByID(st, activation.ChildIDs[0]).GitOpsResult.CommitSHA = strings.Repeat("e", 40)
			}, "activation": func(st *domain.State) { operationByID(st, activation.ID).DevActivation.CandidateHash = "forged" }, "cancel": func(st *domain.State) { operationByID(st, activation.ID).Status = "CANCELLED" }, "quarantine": func(st *domain.State) { operationByID(st, op.ID).ReservationQuarantined = true }, "config": func(st *domain.State) { environmentBinding(st, "dev", "component").Path = "other.yaml" }, "grant": func(st *domain.State) { st.ConnectionGrants = nil }} {
				t.Run(name, func(t *testing.T) {
					copy := cloneRuntimeState(state)
					mutate(&copy)
					if f.s.devRuntimeAllowed(&copy, *operationByID(&copy, op.ID), true) == nil {
						t.Fatal("changed authority retained runtime rights")
					}
				})
			}
			for _, change := range []func(*domain.DevRuntimeProof){func(p *domain.DevRuntimeProof) { p.Generation++ }, func(p *domain.DevRuntimeProof) { p.Components[0].Scopes = nil }, func(p *domain.DevRuntimeProof) { p.Components[0].ImageRepository = "wrong" }} {
				copy := cloneRuntimeState(state)
				v := operationByID(&copy, op.ID)
				change(v.DevRuntime)
				if f.s.devRuntimeAllowed(&copy, *v, true) == nil {
					t.Fatal("damaged expected proof accepted")
				}
			}
			// Key namespaces cannot cross owners, Products or other command classes.
			conflict := command
			conflict.Actor = "other"
			if _, e = f.s.Execute(ctx, conflict); e == nil {
				t.Fatal("foreign owner replay")
			}
			conflict = command
			conflict.ProductID = "other"
			if _, e = f.s.Execute(ctx, conflict); e == nil {
				t.Fatal("foreign Product replay")
			}
			conflict = command
			conflict.Data = map[string]any{"activation_operation_id": activation.ID, "request_key": command.Data["request_key"], "expected_selection": domain.EnvironmentSelection{Generation: 99}}
			if _, e = f.s.Execute(ctx, conflict); e == nil {
				t.Fatal("conflicting payload replay")
			}
			copy := cloneRuntimeState(state)
			operationByID(&copy, op.ID).DevRuntime.Generation = math.MaxUint64
			_ = f.store.Update(ctx, func(st *domain.State) error { *st = copy; return nil })
			next := runtimeVerifyCommand(activation, "runtime-exhausted-next-0002")
			if _, e = f.s.Execute(ctx, next); e == nil {
				t.Fatal("generation exhaustion accepted")
			}
		})
	}
}

func TestDevRuntimeRoleAndGitOpsNegativeEvidence(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, p, activation, observers := devRuntimeFixture(t, kind, false)
			base := observers[0].value
			var index int
			cases := map[string]func(){"wrong-role": func() { observers[0].value.Container = "foreign" }, "wrong-host": func() { observers[0].value.WorkloadImage = "foreign.invalid/x@" + p.digests["dev.yaml"] }, "generation": func() { observers[0].value.ObservedGeneration = 1 }, "no-pods": func() { observers[0].value.Pods = nil }, "replicas": func() { observers[0].value.DesiredReplicas = 2 }, "zero": func() { observers[0].value.DesiredReplicas = 0 }, "unready": func() { observers[0].value.Pods[0].Ready = false }, "pending": func() { observers[0].value.Pods[0].Phase = "Pending" }, "empty-uid": func() { observers[0].value.Pods[0].UID = "" }, "different-imageid": func() { observers[0].value.Pods[0].ImageID = "sha256:" + strings.Repeat("f", 64) }, "mutable": func() { observers[0].value.WorkloadImage = strings.Split(base.WorkloadImage, "@")[0] + ":latest" }, "flux-suffix": func() {
				observers[0].value.Flux = &delivery.RuntimeFluxSnapshot{Status: "READY", Revision: "foreign@sha1:" + p.head}
			}, "flux-unknown": func() { observers[0].value.Flux = nil }, "health": func() { observers[0].value.Health = "NOT_READY" }, "read-error": func() { observers[0].value.Errors = []string{"rawsecret"} }, "duplicate-uid": func() {
				observers[0].value.DesiredReplicas = 2
				observers[0].value.Pods = append(observers[0].value.Pods, observers[0].value.Pods[0])
			}, "gitops-read": func() { p.failRead = true }, "ancestry": func() { p.foreignAncestry = true }, "gitops-missing": func() { p.digests["dev.yaml"] = "" }, "head-drift": func() {
				p.onRevision = func() {
					if p.revisions%2 == 0 {
						p.head = strings.Repeat("f", 40)
					}
				}
			}, "blob-drift": func() { observers[1].onSnapshot = func() { p.blob = strings.Repeat("e", 40) } }}
			// The actual configured path is fixture-defined; use it for target-drift cases.
			st, _ := f.store.Read(context.Background())
			path := operationByID(&st, activation.ChildIDs[0]).Snapshot.Target.Path
			digest := p.digests[path]
			delete(cases, "gitops-missing")
			cases["gitops-missing"] = func() { p.digests[path] = "" }
			for name, mutate := range cases {
				t.Run(name, func(t *testing.T) {
					index++
					observers[0].value = base
					observers[0].value.Pods = append([]delivery.RuntimePod(nil), base.Pods...)
					p.failRead = false
					p.foreignAncestry = false
					p.onRead = nil
					p.onRevision = nil
					p.blob = ""
					p.head = strings.Repeat("c", 40)
					p.digests[path] = digest
					observers[1].onSnapshot = nil
					mutate()
					op := runRuntimeVerification(t, f, runtimeVerifyCommand(activation, fmt.Sprintf("runtime-negative-%02d-%s", index, name)))
					if op.Status != "FAILED" || op.DevRuntimeResult == nil || op.DevRuntimeResult.Outcome != "UNVERIFIED" || runtimeQuery(t, f)["candidate_runtime_verified"] != false {
						t.Fatal("bad evidence verified", op.Status, op.Detail)
					}
					b, _ := json.Marshal(op.DevRuntimeResult)
					if strings.Contains(string(b), "rawsecret") {
						t.Fatal("persisted raw error")
					}
				})
			}
		})
	}
}

func TestDevRuntimeConcurrentGenerationsAndInflightInvalidation(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, activation, observers := devRuntimeFixture(t, kind, false)
			command := runtimeVerifyCommand(activation, "runtime-concurrent-same-key-0001")
			var wg sync.WaitGroup
			ids := make(chan string, 8)
			errs := make(chan error, 8)
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					v, e := f.s.Execute(ctx, command)
					if e != nil {
						errs <- e
						return
					}
					ids <- v.(domain.ExternalOperation).ID
				}()
			}
			wg.Wait()
			close(ids)
			close(errs)
			for e := range errs {
				t.Fatal(e)
			}
			same := ""
			for v := range ids {
				if same != "" && same != v {
					t.Fatal("concurrent replay forked")
				}
				same = v
			}
			var newer domain.ExternalOperation
			observers[0].onSnapshot = func() {
				observers[0].onSnapshot = nil
				v, e := f.s.Execute(ctx, runtimeVerifyCommand(activation, "runtime-newer-pending-old-clock-0002"))
				if e != nil {
					t.Fatal(e)
				}
				newer = v.(domain.ExternalOperation)
				_ = f.store.Update(ctx, func(st *domain.State) error {
					old := operationByID(st, same)
					next := operationByID(st, newer.ID)
					next.CreatedAt = old.CreatedAt
					next.UpdatedAt = old.UpdatedAt
					return nil
				})
			}
			nativeTick(t, f)
			st, _ := f.store.Read(ctx)
			old := operationByID(&st, same)
			if old.Status != "FAILED" || newer.DevRuntime.Generation != 2 || runtimeQuery(t, f)["candidate_runtime_verified"] != false {
				t.Fatal("late older observation defeated latest pending", old.Status, newer.DevRuntime)
			}
			observers[0].value.Health = "NOT_READY"
			nativeTick(t, f)
			st, _ = f.store.Read(ctx)
			if operationByID(&st, newer.ID).Status != "FAILED" || runtimeQuery(t, f)["candidate_runtime_verified"] != false {
				t.Fatal("newer FAIL fell back")
			}
			observers[0].value.Health = "HEALTHY"
			good := runRuntimeVerification(t, f, runtimeVerifyCommand(activation, "runtime-newest-current-0003"))
			if good.Status != "SUCCEEDED" || good.DevRuntime.Generation != 3 || runtimeQuery(t, f)["candidate_runtime_verified"] != true {
				t.Fatal("newest good verification", good.Status)
			}
			for _, mode := range []string{"lease", "coverage", "binding", "gate"} {
				t.Run(mode, func(t *testing.T) {
					original := append([]delivery.RuntimeSnapshotObserver(nil), f.s.runtimeObservers...)
					saved := observers[0].scope
					c := runtimeVerifyCommand(activation, "runtime-inflight-"+mode+"-0004")
					v, e := f.s.Execute(ctx, c)
					if e != nil {
						t.Fatal(e)
					}
					id := v.(domain.ExternalOperation).ID
					observers[0].onSnapshot = func() {
						observers[0].onSnapshot = nil
						switch mode {
						case "lease":
							_ = f.store.Update(ctx, func(st *domain.State) error { operationByID(st, id).LeaseUntil = time.Unix(1, 0); return nil })
						case "coverage":
							f.s.SetRuntimeObservers(original[:1])
						case "binding":
							observers[0].scope.BindingHash = strings.Repeat("b", 64)
						case "gate":
							_ = f.store.Update(ctx, func(st *domain.State) error {
								r := st.Results[len(st.Results)-1]
								r.ID = "runtime-inflight-fail"
								r.Result = "failed"
								r.CreatedAt = time.Unix(1, 0)
								st.Results = append(st.Results, r)
								return nil
							})
						}
					}
					nativeTick(t, f)
					st, _ := f.store.Read(ctx)
					current := operationByID(&st, id)
					if current.Status != "FAILED" || runtimeQuery(t, f)["candidate_runtime_verified"] != false {
						t.Fatal("late invalid evidence accepted", mode, current.Status)
					}
					f.s.SetRuntimeObservers(original)
					observers[0].scope = saved
				})
			}
			_ = p
		})
	}
}

func TestDevRuntimeSequentialSiblingCommitsAndSharedPodContainers(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, p, activation, observers := devRuntimeFixture(t, kind, true)
			before, _ := f.store.Read(context.Background())
			first := operationByID(&before, activation.ChildIDs[0])
			second := operationByID(&before, activation.ChildIDs[1])
			if first.Snapshot.Target.Path != second.Snapshot.Target.Path || first.Snapshot.GitOpsConnection.ID == second.Snapshot.GitOpsConnection.ID {
				t.Fatal("fixture did not share physical path across connection aliases")
			}
			if first.GitOpsResult.CommitSHA == second.GitOpsResult.CommitSHA {
				t.Fatal("fixture did not produce sequential distinct commits")
			}
			// One Deployment with two separately configured containers legitimately shares
			// the pod UID. Both roles must still have exact immutable images and coverage.
			observers[1].scope.Deployment = observers[0].scope.Deployment
			observers[1].value.Deployment = observers[0].value.Deployment
			observers[1].value.Pods[0].Name = observers[0].value.Pods[0].Name
			observers[1].value.Pods[0].UID = observers[0].value.Pods[0].UID
			for _, o := range observers {
				parts := strings.Split(o.value.WorkloadImage, "@")
				o.value.WorkloadImage = parts[0] + ":rcp@" + parts[1]
				o.value.Pods[0].Image = o.value.WorkloadImage
			}
			p.blob = strings.Repeat("e", 40)
			op := runRuntimeVerification(t, f, runtimeVerifyCommand(activation, "runtime-sequential-C1-C2-0001"))
			if op.Status != "SUCCEEDED" || len(op.DevRuntimeResult.Components) != 2 || p.revisions != 2 || runtimeQuery(t, f)["candidate_runtime_verified"] != true {
				t.Fatal("sequential sibling verification failed", op.Status, op.Detail)
			}
			result := op.DevRuntimeResult
			if len(result.Components[0].Revision.AncestorSHAs) != 2 || result.Components[0].GitOps.HeadSHA != p.head || result.Components[1].GitOps.HeadSHA != p.head {
				t.Fatal("did not use one shared current head/all ancestors")
			}
			for name, mutate := range map[string]func(*domain.DevRuntimeResult){"missing-component": func(r *domain.DevRuntimeResult) { r.Components = r.Components[:1] }, "foreign-ancestry": func(r *domain.DevRuntimeResult) {
				r.Components[0].Revision.AncestorSHAs = []string{strings.Repeat("e", 40)}
			}, "sibling-head": func(r *domain.DevRuntimeResult) { r.Components[1].GitOps.HeadSHA = strings.Repeat("e", 40) }, "missing-role": func(r *domain.DevRuntimeResult) { r.Components[0].Roles = r.Components[0].Roles[:1] }} {
				t.Run(name, func(t *testing.T) {
					b, _ := json.Marshal(result)
					var copy domain.DevRuntimeResult
					_ = json.Unmarshal(b, &copy)
					mutate(&copy)
					copy.Hash = devRuntimeResultHash(copy)
					if validateDevRuntimeResult(*op.DevRuntime, copy) == nil {
						t.Fatal("incomplete sibling proof accepted")
					}
				})
			}
			// 64-character Git object IDs remain supported by the common models.
			c := op.DevRuntime.Components[0]
			scope := c.Scopes[0]
			r := result.Components[0].Roles[0]
			head := strings.Repeat("a", 64)
			r.Flux = &delivery.RuntimeFluxSnapshot{Status: "READY", Revision: head}
			if validateRuntimeRole(c, scope, r, head) != nil {
				t.Fatal("valid 64-character Git SHA rejected")
			}
		})
	}
}

func TestDevRuntimeBackupQuarantineAndProofFreshness(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, activation, _ := devRuntimeFixture(t, kind, false)
			c := runtimeVerifyCommand(activation, "runtime-backup-checkpoint-0001")
			op := runRuntimeVerification(t, f, c)
			if op.Status != "SUCCEEDED" {
				t.Fatal(op.Detail)
			}
			state, _ := f.store.Read(ctx)
			for name, mutate := range map[string]func(*domain.ExternalOperation){"age": func(v *domain.ExternalOperation) {
				v.DevRuntimeResult.ObservedAt = time.Now().Add(-6 * time.Minute)
				v.DevRuntimeResult.Hash = devRuntimeResultHash(*v.DevRuntimeResult)
			}, "future": func(v *domain.ExternalOperation) {
				v.DevRuntimeResult.ObservedAt = time.Now().Add(time.Minute)
				v.DevRuntimeResult.Hash = devRuntimeResultHash(*v.DevRuntimeResult)
			}, "hash": func(v *domain.ExternalOperation) { v.DevRuntimeResult.Hash = "forged" }, "role": func(v *domain.ExternalOperation) {
				v.DevRuntimeResult.Components[0].Roles[0].Pods[0].ImageID = "wrong"
				v.DevRuntimeResult.Hash = devRuntimeResultHash(*v.DevRuntimeResult)
			}, "expected": func(v *domain.ExternalOperation) {
				v.DevRuntime.Components[0].Scopes = nil
				v.DevRuntime.SnapshotHash = devRuntimeProofHash(*v.DevRuntime)
			}} {
				t.Run(name, func(t *testing.T) {
					copy := cloneRuntimeState(state)
					mutate(operationByID(&copy, op.ID))
					if f.s.candidateRuntimeState(&copy, "dev", activation.ID)["candidate_runtime_verified"] != false {
						t.Fatal("stale/damaged proof accepted")
					}
				})
			}
			archive, e := f.s.CreateBackup(ctx, "operator")
			if e != nil {
				t.Fatal(e)
			}
			destStore, _ := openWorkerTestStore(t, kind)
			defer destStore.Close()
			dest := NewWithProvider(destStore, p)
			dest.ConfigureWorkers(f.s.externalWorkers)
			dest.SetRuntimeObservers(f.s.runtimeObservers)
			if _, e = dest.RestoreBackup(ctx, "restore-owner", archive.ArchiveBase64, archive.SHA256); e != nil {
				t.Fatal(e)
			}
			restored, _ := destStore.Read(ctx)
			if !operationByID(&restored, op.ID).ReservationQuarantined || activeDevReservation(&restored, "dev") == nil || dest.candidateRuntimeState(&restored, "dev", activation.ID)["candidate_runtime_verified"] != false {
				t.Fatal("restored proof minted current truth")
			}
			reads := p.reads
			if v, e := dest.Execute(ctx, c); e != nil || v.(domain.ExternalOperation).ID != op.ID || p.reads != reads {
				t.Fatal("restored historical replay", e)
			}
			portable, e := f.s.ExportWorkspace(ctx, "p")
			if e != nil {
				t.Fatal(e)
			}
			b, _ := json.Marshal(portable)
			if strings.Contains(string(b), "dev_runtime") || strings.Contains(string(b), op.ID) {
				t.Fatal("portable workspace retained runtime authority")
			}
		})
	}
}

func TestDevRuntimeBoundsAndExactImageReferences(t *testing.T) {
	repo := "registry.example:5000/team/app"
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, v := range []string{repo + "@" + digest, repo + ":rcp@" + digest, repo + ":v1.2@" + digest} {
		if !exactRuntimeImage(v, repo, digest) {
			t.Fatal("valid immutable ref rejected", v)
		}
	}
	for _, v := range []string{repo + ":latest", "foreign/" + repo + "@" + digest, repo + "@" + digest + "@" + digest, repo + ":bad/tag@" + digest, repo + ":@" + digest, repo + "@sha256:" + strings.Repeat("b", 64)} {
		if exactRuntimeImage(v, repo, digest) {
			t.Fatal("wrong/mutable ref accepted", v)
		}
	}
	f, _, activation, observers := devRuntimeFixture(t, "local", false)
	c := runtimeVerifyCommand(activation, "runtime-bound-pods-check-0001")
	observers[0].value.DesiredReplicas = 513
	observers[0].value.Pods = nil
	for i := range 513 {
		pod := observers[1].value.Pods[0]
		pod.Name = fmt.Sprint(i)
		pod.UID = pod.Name
		observers[0].value.Pods = append(observers[0].value.Pods, pod)
	}
	if op := runRuntimeVerification(t, f, c); op.Status != "FAILED" {
		t.Fatal("oversized pod set verified")
	}
	// Proof bytes are bounded including expected coverage, never truncated.
	p := domain.DevRuntimeProof{SnapshotHash: "hash", Components: []domain.DevRuntimeComponent{}}
	r := domain.DevRuntimeResult{ExpectedHash: p.SnapshotHash, Outcome: "VERIFIED", ObservedAt: time.Now()}
	r.Hash = workerconfig.Fingerprint(r)
	if validateDevRuntimeResult(p, r) == nil {
		t.Fatal("malformed empty proof verified")
	}
}

func TestDevRuntimePhysicalGitOpsAuthorityIsolation(t *testing.T) {
	defaultHash, e := runtimeGitOpsAuthority(delivery.Connection{})
	if e != nil {
		t.Fatal(e)
	}
	for _, endpoint := range []string{"https://api.github.com", "https://api.github.com/", "https://API.GITHUB.COM/", "https://api.github.com:443/"} {
		hash, e := runtimeGitOpsAuthority(delivery.Connection{APIURL: endpoint})
		if e != nil || hash != defaultHash {
			t.Fatal("same physical API aliases split", endpoint, e)
		}
	}
	for _, endpoint := range []string{"https://foreign.github.invalid", "https://api.github.com/api/v3"} {
		hash, e := runtimeGitOpsAuthority(delivery.Connection{APIURL: endpoint})
		if e != nil || hash == defaultHash {
			t.Fatal("different physical authority conflated")
		}
	}
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			const foreign = "https://foreign.github.invalid/api/v3"
			f, p, activation, _ := devRuntimeFixture(t, kind, true, foreign)
			p.unprovableEndpoint = foreign
			op := runRuntimeVerification(t, f, runtimeVerifyCommand(activation, "runtime-foreign-physical-api-0001"))
			if op.Status != "FAILED" || op.DevRuntimeResult.Reason != "GITOPS_ANCESTRY_UNVERIFIED" || p.revisions != 2 || runtimeQuery(t, f)["candidate_runtime_verified"] != false {
				t.Fatal("first API ancestry certified foreign numeric repository", op.Status, op.DevRuntimeResult.Reason, p.revisions)
			}
		})
	}
}

func TestDevRuntimeFailureEnvelopeReservesBound(t *testing.T) {
	f, p, activation, original := devRuntimeFixture(t, "local", false)
	st, _ := f.store.Read(context.Background())
	command := runtimeVerifyCommand(activation, "runtime-near-proof-byte-bound-0001")
	in, _, e := devRuntimeInput(command)
	if e != nil {
		t.Fatal(e)
	}
	configure := func(padding int) {
		roles := []delivery.RuntimeSnapshotObserver{}
		for i := range 64 {
			o := *original[0]
			o.scope.Deployment = fmt.Sprintf("role-%02d-", i) + strings.Repeat("d", padding)
			o.scope.Container = strings.Repeat("c", padding)
			o.scope.FluxNamespace = strings.Repeat("f", padding)
			o.scope.Kustomization = strings.Repeat("k", padding)
			o.scope.VerificationGroup = strings.Repeat("g", padding)
			roles = append(roles, &o)
		}
		f.s.SetRuntimeObservers(roles)
	}
	low, high := 1, 1000
	for low < high {
		middle := (low + high + 1) / 2
		configure(middle)
		if _, e := f.s.runtimeExpectedProof(&st, &activation, in, 1); e == nil {
			low = middle
		} else {
			high = middle - 1
		}
	}
	configure(low)
	proof, e := f.s.runtimeExpectedProof(&st, &activation, in, 1)
	if e != nil {
		t.Fatal(e)
	}
	if low < 1 {
		t.Fatal("no bounded proof available")
	}
	configure(low + 1)
	if _, e = f.s.runtimeExpectedProof(&st, &activation, in, 1); e == nil {
		t.Fatal("proof bound did not include failure overhead")
	}
	configure(low)
	p.failRead = true
	op := runRuntimeVerification(t, f, command)
	if op.Status != "FAILED" || len(op.DevRuntime.Components[0].Scopes) != 64 {
		t.Fatal("failed proof dropped roles or did not terminate")
	}
	encoded, _ := json.Marshal(struct {
		Proof  domain.DevRuntimeProof
		Result domain.DevRuntimeResult
	}{proof, *op.DevRuntimeResult})
	if len(encoded) > devRuntimeMaxBytes || len(encoded) < devRuntimeMaxBytes-2048 {
		t.Fatal("failure envelope bound not verified near limit", len(encoded))
	}
	// A 65th role cannot be silently dropped into a certifiable group.
	roles := append([]delivery.RuntimeSnapshotObserver(nil), f.s.runtimeObservers...)
	extra := *original[0]
	extra.scope.Deployment = "sixty-fifth-role"
	f.s.SetRuntimeObservers(append(roles, &extra))
	if _, e = f.s.runtimeCoverage("p", "dev", "component", "local", "dev"); e == nil {
		t.Fatal("65 configured roles accepted")
	}
}

func TestDevRuntimeMalformedLatestNeverFallsBackOrPanics(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, provider, activation, _ := devRuntimeFixture(t, kind, false)
			good := runRuntimeVerification(t, f, runtimeVerifyCommand(activation, "runtime-before-malformed-0001"))
			if good.Status != "SUCCEEDED" {
				t.Fatal(good.Detail)
			}
			base, _ := f.store.Read(context.Background())
			for _, mode := range []string{"missing-proof", "damaged-proof", "cancelled", "quarantined"} {
				t.Run(mode, func(t *testing.T) {
					// Each corruption control starts from a separate empty fixture
					// store. Never truncate append-only event history in-place.
					store, _ := openWorkerTestStore(t, kind)
					defer store.Close()
					if e := store.Update(context.Background(), func(st *domain.State) error { *st = cloneRuntimeState(base); return nil }); e != nil {
						t.Fatal(e)
					}
					isolated := *f
					isolated.store = store
					isolated.s = NewWithProvider(store, provider)
					isolated.s.ConfigureWorkers(f.s.externalWorkers)
					isolated.s.SetRuntimeObservers(f.s.runtimeObservers)
					f := &isolated
					v, e := f.s.Execute(context.Background(), runtimeVerifyCommand(activation, "runtime-malformed-"+mode+"-0002"))
					if e != nil {
						t.Fatal(e)
					}
					id := v.(domain.ExternalOperation).ID
					if e = f.store.Update(context.Background(), func(st *domain.State) error {
						op := operationByID(st, id)
						switch mode {
						case "missing-proof":
							op.DevRuntime = nil
						case "damaged-proof":
							op.DevRuntime.SnapshotHash = "forged"
						case "cancelled":
							op.Status = "CANCELLED"
						case "quarantined":
							op.ReservationQuarantined = true
						}
						return nil
					}); e != nil {
						t.Fatal(e)
					}
					if runtimeQuery(t, f)["candidate_runtime_verified"] != false {
						t.Fatal("malformed latest fell back to PASS")
					}
					nativeTick(t, f)
					after, _ := f.store.Read(context.Background())
					op := operationByID(&after, id)
					if mode != "cancelled" && op.Status != "FAILED" {
						t.Fatal("malformed proof was not safely terminated", op.Status)
					}
					if runtimeQuery(t, f)["candidate_runtime_verified"] != false {
						t.Fatal("malformed latest result fell back")
					}
				})
			}
		})
	}
}

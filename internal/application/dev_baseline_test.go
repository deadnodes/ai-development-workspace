package application

import (
	"context"
	"encoding/json"
	"os"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func (p *devActivationProvider) VerifyGitOpsRevision(_ context.Context, _ delivery.Connection, r delivery.GitOpsRevisionRequest) (delivery.GitOpsRevisionProof, error) {
	head := p.baselineHead
	if head == "" {
		head = strings.Repeat("b", 40)
	}
	if r.HeadSHA != "" && r.HeadSHA != head {
		return delivery.GitOpsRevisionProof{}, invalid("fixture baseline branch changed")
	}
	return delivery.GitOpsRevisionProof{Repository: r.Repository, Ref: r.Ref, HeadSHA: head, AncestorSHAs: r.AncestorSHAs}, nil
}
func (p *devActivationUnguardedProvider) VerifyGitOpsRevision(ctx context.Context, c delivery.Connection, r delivery.GitOpsRevisionRequest) (delivery.GitOpsRevisionProof, error) {
	return p.baselineReader.VerifyGitOpsRevision(ctx, c, r)
}
func (p *devActivationUnguardedProvider) ReadGitOps(ctx context.Context, c delivery.Connection, r delivery.GitOpsRequest) (delivery.GitOpsSnapshot, error) {
	return p.baselineReader.ReadGitOps(ctx, c, r)
}
func baselineFixtureCommand(hold domain.ExternalOperation, key string) domain.Command {
	return domain.Command{Action: "capture_candidate_dev_baseline", ProductID: hold.ProductID, Actor: hold.RequestedBy, Data: map[string]any{"reservation_operation_id": hold.ID, "request_key": key, "expected_selection": hold.DevReservation.ReservedSelection}}
}

// The unit-test registry peer below is an authenticated worker attestation fixture;
// the root pipeline separately exercises the actual Distribution scanner.
func prepareActivationFixtureBaseline(t *testing.T, f *nativeFixture, p *devActivationProvider, hold domain.ExternalOperation) domain.ExternalOperation {
	t.Helper()
	ctx := context.Background()
	st, e := f.store.Read(ctx)
	if e != nil {
		t.Fatal(e)
	}
	comp := compositionByID(&st, hold.DevReservation.BaselineSelection.CompositionID)
	build, _ := f.s.externalWorkers.Recipe("build")
	pubs := []workerconfig.PublicationRecipe{}
	seen := map[string]bool{}
	for _, j := range st.WorkerJobs {
		if j.Capability == "registry_publish" && !seen[j.Inputs.RecipeID] {
			r, ok := f.s.externalWorkers.Publication(j.Inputs.RecipeID)
			if ok {
				pubs = append(pubs, r)
				seen[r.ID] = true
			}
		}
	}
	for _, binding := range st.ComponentBuilds {
		if binding.Native != nil && !seen[binding.Native.PublicationRecipeID] {
			r, ok := f.s.externalWorkers.Publication(binding.Native.PublicationRecipeID)
			if ok {
				pubs = append(pubs, r)
				seen[r.ID] = true
			}
		}
	}
	if len(pubs) == 0 {
		pubs = append(pubs, f.recipe)
	}
	t.Setenv("REGISTRY_FIXTURE_TOKEN", registryTestToken)
	cfg := workerconfig.Config{Revision: f.s.externalWorkers.Revision(), LeaseSeconds: 5, Recipes: []workerconfig.Recipe{build}, Publications: pubs, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "NATIVE_BUILD_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "NATIVE_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "observer", Principal: "worker/observer", TokenEnv: "REGISTRY_FIXTURE_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_observe"}, ExpiresAt: time.Now().Add(time.Hour)}}}
	p.baselineHead = strings.Repeat("b", 40)
	p.baselineImages = map[string]string{}
	p.baselineDigests = map[string]string{}
	observers := []delivery.RuntimeSnapshotObserver{}
	for i, part := range comp.Components {
		app := applicationByID(&st, part.ApplicationID)
		current := componentBuild(&st, part.ApplicationID)
		target := environmentBinding(&st, hold.EnvironmentID, part.ApplicationID)
		recipe := f.recipe
		for _, r := range pubs {
			if r.ComponentID == app.ID {
				recipe = r
			}
		}
		observe := registryRecipe()
		observe.ID = "baseline-observe-" + app.ID
		observe.ComponentID = app.ID
		observe.RepositoryID = app.RepositoryID
		observe.Origin = recipe.Origin
		observe.Repository = recipe.Repository
		observe.Pull = recipe.Pull
		observe.Hash = workerconfig.ObservationHash(observe)
		cfg.Observations = append(cfg.Observations, observe)
		digest := registryDigest(900 + i)
		key := target.Path + "|" + current.ImageRepository
		p.baselineImages[key] = current.ImageRepository
		p.baselineDigests[key] = digest
		for _, role := range []string{"http", "worker"} {
			scope := delivery.RuntimeVerificationScope{ProductID: hold.ProductID, EnvironmentID: hold.EnvironmentID, ApplicationID: app.ID, Context: hold.DevReservation.Environment.Cluster, Namespace: hold.DevReservation.Environment.Namespace, Deployment: app.ID + "-workload", Container: role, FluxNamespace: "flux-system", Kustomization: "apps-dev", VerificationGroup: "complete-v1", BindingHash: strings.Repeat("a", 64)}
			image := current.ImageRepository + "@" + digest
			o := &devRuntimeObserver{scope: scope, value: delivery.RuntimeSnapshot{Context: scope.Context, ProductID: scope.ProductID, EnvironmentID: scope.EnvironmentID, ApplicationID: scope.ApplicationID, Namespace: scope.Namespace, Deployment: scope.Deployment, Container: scope.Container, Generation: 2, ObservedGeneration: 2, DesiredReplicas: 1, Health: "HEALTHY", WorkloadImage: image, Pods: []delivery.RuntimePod{{Name: scope.Deployment + "-pod", UID: scope.Deployment + "-uid", Phase: "Running", Image: image, ImageID: "containerd://" + digest, Ready: true}}, Flux: &delivery.RuntimeFluxSnapshot{Status: "READY", Revision: "main@sha1:" + p.baselineHead}}}
			observers = append(observers, o)
		}
	}
	authority, e := workerconfig.New(cfg, "native-control-fixture", os.Getenv)
	if e != nil {
		t.Fatal(e)
	}
	f.s.ConfigureWorkers(authority)
	f.p, _ = authority.Authenticate(nativeToken, time.Now())
	f.s.SetRuntimeObservers(observers)
	value, e := f.s.Execute(ctx, baselineFixtureCommand(hold, "fixture-baseline-"+hold.ID))
	if e != nil {
		t.Fatal(e)
	}
	capture := value.(domain.ExternalOperation)
	if _, e = f.s.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	for range len(comp.Components) {
		principal := registryPrincipal(t, f.s)
		lease, e := f.s.ClaimWorker(ctx, principal, domain.WorkerClaim{ProductID: hold.ProductID, RepositoryID: "source", Capability: "registry_observe"})
		if e != nil || lease == nil {
			t.Fatal("causal baseline registry claim", e)
		}
		started, e := f.s.StartWorker(ctx, principal, workerRequest(lease))
		if e != nil {
			t.Fatal(e)
		}
		report := baselineRegistryFixtureReport(t, &started)
		if _, e = f.s.ReportWorker(ctx, principal, report); e != nil {
			t.Fatal(e)
		}
	}
	for range 3 {
		nativeTick(t, f)
		state, _ := f.store.Read(ctx)
		capture = *operationByID(&state, capture.ID)
		if terminalOperation(capture.Status) {
			break
		}
	}
	if capture.Status != "SUCCEEDED" || capture.Phase != "BASELINE_CAPTURED" {
		t.Fatal("fixture full baseline capture", capture.Status, capture.Phase, capture.Detail)
	}
	return capture
}

// This fixture models explicitly attested pull bytes while retaining unknown
// incoming referrers. Generic inventory reports intentionally have no such proof.
func baselineRegistryFixtureReport(t *testing.T, lease *domain.WorkerLease) domain.WorkerReport {
	t.Helper()
	r := registryReport(t, lease, true)
	g := &r.Registry.Graph
	for _, root := range g.Roots {
		var manifest domain.RegistryNode
		for _, n := range g.Nodes {
			if n.Digest == root.Digest {
				manifest = n
			}
		}
		desc := []domain.RegistryImageContentDescriptor{{Digest: registryDigest(200), Kind: "CONFIG", MediaType: "application/vnd.oci.image.config.v1+json", Size: 32}}
		proof := domain.RegistryImageContentRoot{Digest: root.Digest, Closure: "COMPLETE", Manifests: []domain.RegistryImageContentManifest{{Digest: root.Digest, Kind: manifest.Kind, MediaType: manifest.MediaType, Size: manifest.Size, DescriptorCount: len(desc), DescriptorHash: domain.RegistryImageContentDescriptorHash(desc), Descriptors: desc}}}
		proof.Hash = domain.RegistryImageContentRootHash(proof)
		g.ImageContentRoots = append(g.ImageContentRoots, proof)
	}
	g.Discovery = "PARTIAL"
	g.Blockers = append(g.Blockers, "registry_referrers_unknown")
	for i := range g.Roots {
		g.Roots[i].Closure = "PARTIAL"
		g.Roots[i].Blockers = []string{"registry_referrers_unknown"}
	}
	var e error
	*g, e = domain.NormalizeRegistryGraph(*g)
	if e != nil {
		t.Fatal(e)
	}
	g.GraphHash = domain.RegistryGraphHash(*g)
	return r
}

func TestDevBaselinePositiveReadOnlyFullCapture(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, _, hold, c := devActivationFixture(t, kind)
			st, e := f.store.Read(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			capture, ok := latestBaseline(&st, hold.ID)
			if !ok || capture == nil || capture.Status != "SUCCEEDED" || len(capture.DevBaselineResult.Components) == 0 {
				t.Fatal("missing complete baseline")
			}
			receipt := capture.DevBaselineResult.Components[0].Registry
			if receipt == nil || receipt.ClosureKind != "IMAGE_CONTENT" || receipt.ContentProofHash == "" || receipt.MetadataRootClosure != "PARTIAL" || receipt.MetadataDiscovery != "PARTIAL" || receipt.GlobalInventoryComplete || !slices.Contains(receipt.MetadataBlockers, "registry_referrers_unknown") {
				t.Fatal("content proof upgraded metadata or was not bound", receipt)
			}
			if f.s.baselineQuery(&st, hold.EnvironmentID)["baseline_capture_current_valid"] != true {
				t.Fatal("baseline query")
			}
			value, e := f.s.Execute(context.Background(), c)
			if e != nil {
				t.Fatal(e)
			}
			activation := value.(domain.ExternalOperation)
			if activation.DevActivation.BaselineCaptureID != capture.ID {
				t.Fatal("activation omittedbaseline")
			}
		})
	}
}

func TestDevBaselineImageContentEvidenceBoundaries(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, _, hold, _ := devActivationFixture(t, kind)
			st, _ := f.store.Read(context.Background())
			capture, _ := latestBaseline(&st, hold.ID)
			c := capture.DevBaseline.Components[0]
			var inventory domain.RegistryInventory
			for _, v := range st.RegistryInventories {
				if v.JobID == c.RegistryJobID {
					inventory = v
				}
			}
			if _, e := baselineRegistryContentHash(inventory, c.Digest, time.Now().UTC()); e != nil {
				t.Fatal(e)
			}
			cases := map[string]func(*domain.RegistryInventory){
				"generic-partial-no-content": func(v *domain.RegistryInventory) { v.Report.Graph.ImageContentRoots = nil },
				"generic-metadata-complete-no-content": func(v *domain.RegistryInventory) {
					v.Report.Graph.ImageContentRoots = nil
					v.Report.Graph.Discovery = "COMPLETE"
					for i := range v.Report.Graph.Roots {
						v.Report.Graph.Roots[i].Closure = "COMPLETE"
						v.Report.Graph.Roots[i].Blockers = []string{}
					}
				},
				"malformed-content-hash": func(v *domain.RegistryInventory) { v.Report.Graph.ImageContentRoots[0].Hash = registryDigest(91) },
				"partial-content": func(v *domain.RegistryInventory) {
					p := &v.Report.Graph.ImageContentRoots[0]
					p.Closure = "PARTIAL"
					p.Manifests = []domain.RegistryImageContentManifest{}
					p.Hash = domain.RegistryImageContentRootHash(*p)
				},
				"omitted-edge": func(v *domain.RegistryInventory) { v.Report.Graph.Edges = nil },
				"omitted-root-node": func(v *domain.RegistryInventory) {
					for i, n := range v.Report.Graph.Nodes {
						if n.Digest == c.Digest {
							v.Report.Graph.Nodes = append(v.Report.Graph.Nodes[:i], v.Report.Graph.Nodes[i+1:]...)
							break
						}
					}
				},
				"missing-config-bytes": func(v *domain.RegistryInventory) {
					for i, n := range v.Report.Graph.Nodes {
						if n.Kind == "BLOB" {
							v.Report.Graph.Nodes[i].Availability = "UNKNOWN"
						}
					}
				},
				"truncated-descriptors": func(v *domain.RegistryInventory) {
					p := &v.Report.Graph.ImageContentRoots[0]
					p.Manifests[0].Descriptors = nil
					p.Hash = domain.RegistryImageContentRootHash(*p)
				},
				"missing-layer": func(v *domain.RegistryInventory) {
					p := &v.Report.Graph.ImageContentRoots[0]
					m := &p.Manifests[0]
					m.Descriptors = append(m.Descriptors, domain.RegistryImageContentDescriptor{Digest: registryDigest(92), Kind: "LAYER", MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Size: 11})
					m.DescriptorCount = len(m.Descriptors)
					m.DescriptorHash = domain.RegistryImageContentDescriptorHash(m.Descriptors)
					p.Hash = domain.RegistryImageContentRootHash(*p)
				},
				"node-limit":    func(v *domain.RegistryInventory) { v.Inputs.Registry.Limits.MaxNodes = 1 },
				"foreign-roots": func(v *domain.RegistryInventory) { v.Inputs.Registry.Roots = []string{registryDigest(93)} },
				"stale":         func(v *domain.RegistryInventory) { v.Report.FinishedAt = time.Now().Add(-24 * time.Hour) },
			}
			for name, mutate := range cases {
				t.Run(name, func(t *testing.T) {
					encoded, _ := json.Marshal(inventory)
					var changed domain.RegistryInventory
					if e := json.Unmarshal(encoded, &changed); e != nil {
						t.Fatal(e)
					}
					mutate(&changed)
					changed.Report.Graph.GraphHash = domain.RegistryGraphHash(changed.Report.Graph)
					if _, e := baselineRegistryContentHash(changed, c.Digest, time.Now().UTC()); e == nil {
						t.Fatal("incomplete or generic evidence certified content")
					}
				})
			}
		})
	}
}

func TestDevBaselineAdmissionReplayGenerationsAndNoProvenance(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, activationCommand := devActivationFixture(t, kind)
			base, _ := f.store.Read(ctx)
			original, _ := latestBaseline(&base, hold.ID)
			if original == nil {
				t.Fatal("missingbaseline")
			}
			artifacts := len(base.DeliveryArtifacts)
			builds := len(base.DeliveryBuildRuns)
			request := baselineFixtureCommand(hold, "baseline-next-generation-0002")
			var ids []string
			for range 2 {
				v, e := f.s.Execute(ctx, request)
				if e != nil {
					t.Fatal(e)
				}
				ids = append(ids, v.(domain.ExternalOperation).ID)
			}
			if ids[0] != ids[1] {
				t.Fatal("replay forked capture")
			}
			after, _ := f.store.Read(ctx)
			latest, _ := latestBaseline(&after, hold.ID)
			if latest.DevBaseline.Generation != 2 || f.s.baselineQuery(&after, hold.EnvironmentID)["baseline_capture_current_valid"] != false {
				t.Fatal("pending capture fell back")
			}
			// Caller-supplied evidence and other actors cannot shape the physical baseline.
			for name, mutate := range map[string]func(*domain.Command){"owner": func(c *domain.Command) { c.Actor = "other" }, "product": func(c *domain.Command) { c.ProductID = "other" }, "unknown": func(c *domain.Command) { c.Data["healthy"] = true }, "case": func(c *domain.Command) { c.Data["Request_Key"] = "x" }, "authority": func(c *domain.Command) { c.ID = "forged" }, "cas": func(c *domain.Command) { c.Data["expected_selection"] = domain.EnvironmentSelection{Generation: 99} }} {
				t.Run(name, func(t *testing.T) {
					c := baselineFixtureCommand(hold, "baseline-invalid-"+name+"-0003")
					mutate(&c)
					if _, e := f.s.Execute(ctx, c); e == nil {
						t.Fatal("invalidcaptureadmitted")
					}
				})
			}
			for range 2 {
				nativeTick(t, f)
			}
			state, _ := f.store.Read(ctx)
			latest = operationByID(&state, ids[0])
			if latest.Status != "RUNNING" || latest.Phase != "WAIT_REGISTRY" {
				t.Fatal("asyncregistrystage", latest.Status, latest.Phase)
			}
			principal := registryPrincipal(t, f.s)
			lease, e := f.s.ClaimWorker(ctx, principal, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_observe"})
			if e != nil || lease == nil {
				t.Fatal(e)
			}
			started, e := f.s.StartWorker(ctx, principal, workerRequest(lease))
			if e != nil {
				t.Fatal(e)
			}
			report := registryReport(t, &started, false)
			if _, e = f.s.ReportWorker(ctx, principal, report); e != nil {
				t.Fatal(e)
			}
			nativeTick(t, f)
			state, _ = f.store.Read(ctx)
			latest = operationByID(&state, ids[0])
			if latest.Status != "FAILED" || f.s.baselineQuery(&state, "dev")["baseline_capture_current_valid"] != false {
				t.Fatal("partialPASSEDscanaccepted")
			}
			if _, e = f.s.Execute(ctx, activationCommand); e == nil || p.physicalWrites != 0 {
				t.Fatal("invalidcapturepermittedmutation")
			}
			if len(state.DeliveryArtifacts) != artifacts || len(state.DeliveryBuildRuns) != builds {
				t.Fatal("restoration-onlyreceiptinventedbuildprovenance")
			}
			// Historical exact replay remains available without fresh rights.
			events := len(state.Events)
			v, e := f.s.Execute(ctx, request)
			if e != nil || v.(domain.ExternalOperation).ID != ids[0] {
				t.Fatal("historicalreplay", e)
			}
			state, _ = f.store.Read(ctx)
			if len(state.Events) != events {
				t.Fatal("replayaudited")
			}
		})
	}
}

func TestDevBaselineCurrentProofAndCausalTamperGuards(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			f, _, hold, _ := devActivationFixture(t, kind)
			ctx := context.Background()
			state, _ := f.store.Read(ctx)
			capture, _ := latestBaseline(&state, hold.ID)
			id := capture.ID
			cases := map[string]func(*domain.State){"epoch": func(st *domain.State) { environmentByID(st, "dev").ReservationEpoch++ }, "cas": func(st *domain.State) { environmentByID(st, "dev").SelectionGeneration++ }, "target": func(st *domain.State) { environmentBinding(st, "dev", "component").Path = "other.yaml" }, "profile": func(st *domain.State) { componentBuild(st, "component").Profile = "other" }, "quarantine": func(st *domain.State) { operationByID(st, id).ReservationQuarantined = true }, "hold": func(st *domain.State) { operationByID(st, hold.ID).RequestedBy = "other" }, "cancel": func(st *domain.State) { operationByID(st, id).Status = "CANCELLED" }, "proof": func(st *domain.State) { operationByID(st, id).DevBaseline.Components[0].Digest = registryDigest(111) }, "registry-cancel": func(st *domain.State) {
				c := operationByID(st, id).DevBaseline.Components[0]
				j := workerJob(st, c.RegistryJobID)
				operationByID(st, j.OperationID).Status = "CANCELLED"
			}, "registry-kind": func(st *domain.State) {
				c := operationByID(st, id).DevBaseline.Components[0]
				j := workerJob(st, c.RegistryJobID)
				operationByID(st, j.OperationID).Kind = "DEPLOY"
			}, "registry-job": func(st *domain.State) {
				c := operationByID(st, id).DevBaseline.Components[0]
				workerJob(st, c.RegistryJobID).Fence++
			}, "registry-receipt": func(st *domain.State) {
				operationByID(st, id).DevBaselineResult.Components[0].Registry.InventoryID = "foreign"
			}, "nestedfuture": func(st *domain.State) {
				o := operationByID(st, id)
				o.DevBaselineResult.Runtime.ObservedAt = o.DevBaselineResult.ObservedAt.Add(time.Minute)
				o.DevBaselineResult.Runtime.Hash = devRuntimeResultHash(*o.DevBaselineResult.Runtime)
				o.DevBaselineResult.Hash = baselineResultHash(*o.DevBaselineResult)
			}, "nestedstale": func(st *domain.State) {
				o := operationByID(st, id)
				o.DevBaselineResult.Runtime.ObservedAt = o.DevBaselineResult.ObservedAt.Add(-6 * time.Minute)
				o.DevBaselineResult.Runtime.Hash = devRuntimeResultHash(*o.DevBaselineResult.Runtime)
				o.DevBaselineResult.Hash = baselineResultHash(*o.DevBaselineResult)
			}, "oldcapture": func(st *domain.State) {
				o := operationByID(st, id)
				o.DevBaselineResult.ObservedAt = time.Now().Add(-6 * time.Minute)
				o.DevBaselineResult.Hash = baselineResultHash(*o.DevBaselineResult)
			}}
			for name, mutate := range cases {
				t.Run(name, func(t *testing.T) {
					copy := cloneRuntimeState(state)
					mutate(&copy)
					if f.s.baselineCurrent(&copy, *operationByID(&copy, id), true) == nil {
						t.Fatal("damagedcurrentcaptureaccepted")
					}
				})
			}
			original := f.s.runtimeObservers
			f.s.SetRuntimeObservers(original[:1])
			if f.s.baselineCurrent(&state, *capture, true) == nil {
				t.Fatal("partialcoverageaccepted")
			}
			f.s.SetRuntimeObservers(original)
			stale := baselineFixtureCommand(hold, "baseline-fresh-staleCAS-0002")
			stale.Data["expected_selection"] = domain.EnvironmentSelection{Generation: 1}
			if _, e := f.s.Execute(ctx, stale); e == nil {
				t.Fatal("staleCAScapture")
			}
		})
	}
}

func TestDevBaselineKnownNativeAvailabilityAndHistoricalMaterial(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, activationCommand := devActivationFixture(t, kind)
			state, _ := f.store.Read(ctx)
			var artifact domain.DeliveryArtifact
			for _, a := range state.DeliveryArtifacts {
				if a.Native != nil && a.ApplicationID == "component" {
					artifact = a
					break
				}
			}
			if artifact.ID == "" {
				t.Fatal("nativeartifactmissing")
			}
			for key := range p.baselineDigests {
				p.baselineDigests[key] = artifact.Digest
			}
			for _, observer := range f.s.runtimeObservers {
				o := observer.(*devRuntimeObserver)
				image := artifact.ImageRepository + "@" + artifact.Digest
				o.value.WorkloadImage = image
				o.value.Pods[0].Image = image
				o.value.Pods[0].ImageID = "containerd://" + artifact.Digest
			}
			beforeArtifacts := len(state.DeliveryArtifacts)
			value, e := f.s.Execute(ctx, baselineFixtureCommand(hold, "baseline-known-native-0002"))
			if e != nil {
				t.Fatal(e)
			}
			id := value.(domain.ExternalOperation).ID
			for range 3 {
				nativeTick(t, f)
			}
			state, _ = f.store.Read(ctx)
			capture := operationByID(&state, id)
			if capture.Status != "SUCCEEDED" || capture.DevBaseline.Components[0].ArtifactID != artifact.ID || len(capture.ChildIDs) != 0 || capture.DevBaselineResult.Components[0].Registry != nil || len(state.DeliveryArtifacts) != beforeArtifacts {
				t.Fatal("knownnativecapture", capture.Status, capture.Detail)
			}
			value, e = f.s.Execute(ctx, activationCommand)
			if e != nil {
				t.Fatal(e)
			}
			activation := value.(domain.ExternalOperation)
			// Once an owned durable effect exists, the original capture remains historical
			// recovery material rather than a fresh-now assertion about the mutated Dev.
			forceActivationTick(t, f, activation.ChildIDs[0])
			forceActivationTick(t, f, activation.ChildIDs[0])
			state, _ = f.store.Read(ctx)
			activation = *operationByID(&state, activation.ID)
			if !activationHasBaselineEffect(&state, activation) {
				t.Fatal("fixtureactivationdidnotreachintent")
			}
			capture = operationByID(&state, id)
			old := capture.DevBaselineResult.ObservedAt
			capture.DevBaselineResult.ObservedAt = old.Add(-time.Hour)
			capture.DevBaselineResult.Hash = baselineResultHash(*capture.DevBaselineResult)
			// Tampering with stored observation history is forbidden, even after effect.
			if f.s.activationBaselineAllowed(&state, activation) == nil {
				t.Fatal("capturebindingdidnotcoverresult")
			}
		})
	}
}

func finishBaselineFixtureRegistryJobs(t *testing.T, f *nativeFixture) {
	t.Helper()
	ctx := context.Background()
	principal := registryPrincipal(t, f.s)
	for range 20 {
		lease, e := f.s.ClaimWorker(ctx, principal, domain.WorkerClaim{ProductID: "p", RepositoryID: "source", Capability: "registry_observe"})
		if e != nil {
			t.Fatal(e)
		}
		if lease == nil {
			return
		}
		started, e := f.s.StartWorker(ctx, principal, workerRequest(lease))
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.s.ReportWorker(ctx, principal, baselineRegistryFixtureReport(t, &started)); e != nil {
			t.Fatal(e)
		}
	}
	t.Fatal("fixturejobbound")
}
func TestDevBaselineConcurrencyLateCompletionAndInflightDrift(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _, hold, _ := devActivationFixture(t, kind)
			command := baselineFixtureCommand(hold, "baseline-concurrent-key-0002")
			ids := make(chan string, 8)
			errs := make(chan error, 8)
			var wg sync.WaitGroup
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
			for id := range ids {
				if same != "" && same != id {
					t.Fatal("concurrentcapturefork")
				}
				same = id
			}
			nativeTick(t, f)
			finishBaselineFixtureRegistryJobs(t, f)
			observer := f.s.runtimeObservers[0].(*devRuntimeObserver)
			var newer domain.ExternalOperation
			observer.onSnapshot = func() {
				observer.onSnapshot = nil
				v, e := f.s.Execute(ctx, baselineFixtureCommand(hold, "baseline-newer-during-old-read-0003"))
				if e != nil {
					t.Fatal(e)
				}
				newer = v.(domain.ExternalOperation)
				if e = f.store.Update(ctx, func(st *domain.State) error {
					operationByID(st, newer.ID).CreatedAt = operationByID(st, same).CreatedAt
					return nil
				}); e != nil {
					t.Fatal(e)
				}
			}
			nativeTick(t, f)
			state, _ := f.store.Read(ctx)
			if operationByID(&state, same).Status != "FAILED" || newer.DevBaseline.Generation != 3 || f.s.baselineQuery(&state, "dev")["baseline_capture_current_valid"] != false {
				t.Fatal("lateolderrevivedbaseline")
			}
			nativeTick(t, f)
			finishBaselineFixtureRegistryJobs(t, f)
			nativeTick(t, f)
			state, _ = f.store.Read(ctx)
			if operationByID(&state, newer.ID).Status != "SUCCEEDED" {
				t.Fatal("newercapture", operationByID(&state, newer.ID).Detail)
			}
			for _, mode := range []string{"lease", "coverage", "binding"} {
				t.Run(mode, func(t *testing.T) {
					v, e := f.s.Execute(ctx, baselineFixtureCommand(hold, "baseline-final-drift-"+mode+"-0004"))
					if e != nil {
						t.Fatal(e)
					}
					id := v.(domain.ExternalOperation).ID
					nativeTick(t, f)
					finishBaselineFixtureRegistryJobs(t, f)
					saved := f.s.runtimeObservers
					scope := observer.scope
					observer.onSnapshot = func() {
						observer.onSnapshot = nil
						switch mode {
						case "lease":
							_ = f.store.Update(ctx, func(st *domain.State) error { operationByID(st, id).LeaseUntil = time.Unix(1, 0); return nil })
						case "coverage":
							f.s.SetRuntimeObservers(saved[:1])
						case "binding":
							observer.scope.BindingHash = strings.Repeat("b", 64)
						}
					}
					nativeTick(t, f)
					state, _ := f.store.Read(ctx)
					if operationByID(&state, id).Status != "FAILED" || f.s.baselineQuery(&state, "dev")["baseline_capture_current_valid"] != false {
						t.Fatal("lateinvalidbaselineaccepted", mode)
					}
					f.s.SetRuntimeObservers(saved)
					observer.scope = scope
				})
			}
		})
	}
}

func TestDevBaselineCompleteComponentsBackupAndExport(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, p, hold, _ := devActivationMultiFixture(t, kind)
			state, _ := f.store.Read(ctx)
			capture, _ := latestBaseline(&state, hold.ID)
			if capture == nil || len(capture.DevBaseline.Components) != 2 || len(capture.DevBaselineResult.Components) != 2 {
				t.Fatal("fullsharedcomponentcoveragemissing")
			}
			for _, c := range capture.DevBaseline.Components {
				if c.Digest == "" || len(c.Scopes) != 2 || c.RegistryJobID == "" {
					t.Fatal("componentomittedfromoriginalbaseline")
				}
			}
			report := f.s.evaluateRetention(state, "p", time.Now().UTC())
			for _, c := range capture.DevBaseline.Components {
				protected := false
				for _, image := range report.Images {
					if image.ImageRepository == c.ImageRepository && image.Digest == c.Digest {
						protected = image.Protected
					}
				}
				if !protected {
					t.Fatal("baselineartifactunprotected")
				}
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
			if !operationByID(&restored, capture.ID).ReservationQuarantined || dest.baselineQuery(&restored, "dev")["baseline_capture_current_valid"] != false {
				t.Fatal("restorecreatedbaselineauthority")
			}
			v, e := dest.Execute(ctx, baselineFixtureCommand(hold, capture.RequestKey))
			if e != nil || v.(domain.ExternalOperation).ID != capture.ID {
				t.Fatal("quarantinedhistoricalreplay", e)
			}
			portable, e := f.s.ExportWorkspace(ctx, "p")
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal(portable)
			if strings.Contains(string(raw), "dev_baseline") || strings.Contains(string(raw), capture.ID) {
				t.Fatal("portableexecutionauthorityleak")
			}
		})
	}
}

func TestDevBaselineIncludesUntouchedApplication(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f, _, reserve := devReservationFixture(t, kind)
			source := f.s.provider.(*mainCandidateProvider)
			build, _ := f.s.externalWorkers.Recipe("build")
			second := f.recipe
			second.ID = "publish-untouched"
			second.ComponentID = "untouched"
			second.Repository += "-untouched"
			second.Hash = workerconfig.PublicationHash(second)
			cfg := workerconfig.Config{Revision: f.s.externalWorkers.Revision(), LeaseSeconds: 5, Recipes: []workerconfig.Recipe{build}, Publications: []workerconfig.PublicationRecipe{f.recipe, second}, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "NATIVE_BUILD_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(time.Hour)}, {ID: "publish", Principal: "worker/publish", TokenEnv: "NATIVE_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(time.Hour)}}}
			authority, e := workerconfig.New(cfg, "native-control-fixture", os.Getenv)
			if e != nil {
				t.Fatal(e)
			}
			f.s.ConfigureWorkers(authority)
			exec(t, f.s, domain.Command{Action: "create_application", ID: "untouched", ProductID: "p", Data: map[string]any{"name": "Untouched B", "repository_id": "source"}})
			exec(t, f.s, domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "untouched", "connection_id": "conn", "mode": "NATIVE", "profile": second.Profile, "image_repository": nativeImage(second), "native": domain.NativeBinding{BuildRecipeID: build.ID, PublicationRecipeID: second.ID, Parameters: map[string]any{}}}})
			exec(t, f.s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "untouched", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "untouched.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}})
			exec(t, f.s, domain.Command{Action: "plan_composition", ID: "full-untouched-baseline", ProductID: "p", Data: map[string]any{"name": "Full A+B physical baseline", "environment_id": "dev", "components": []domain.CompositionComponent{component("component", "rev"), component("untouched", "rev")}}})
			exec(t, f.s, domain.Command{Action: "select_composition", ID: "full-untouched-baseline"})
			state, _ := f.store.Read(ctx)
			reserve.Data["expected_selection"] = selectionToken(*environmentByID(&state, "dev"))
			v, e := f.s.Execute(ctx, reserve)
			if e != nil {
				t.Fatal(e)
			}
			hold := v.(domain.ExternalOperation)
			p := &devActivationProvider{mainCandidateProvider: source}
			f.s.provider = p
			capture := prepareActivationFixtureBaseline(t, f, p, hold)
			if len(capture.DevBaseline.Components) != 2 || len(hold.DevReservation.Targets) != 1 {
				t.Fatal("untouched baseline application not captured independently of candidate")
			}
			candidate := operationByID(&state, hold.DevReservation.CandidateOperationID)
			if candidate == nil || len(candidate.ChildIDs) != 1 {
				t.Fatal("fixture candidate unexpectedly includes B")
			}
			v, e = f.s.Execute(ctx, domain.Command{Action: "deploy_candidate_dev", ProductID: "p", Actor: hold.RequestedBy, Data: map[string]any{"reservation_operation_id": hold.ID, "request_key": "baseline-activate-A-only-0001", "expected_selection": hold.DevReservation.ReservedSelection}})
			if e != nil {
				t.Fatal(e)
			}
			activation := v.(domain.ExternalOperation)
			if len(activation.ChildIDs) != 1 {
				t.Fatal("untouched B gained candidate execution")
			}
			for _, c := range capture.DevBaseline.Components {
				if c.ApplicationID == "untouched" && c.Digest == "" {
					t.Fatal("untouched B lacks immutable recovery bytes")
				}
			}
		})
	}
}

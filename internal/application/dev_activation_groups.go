package application

import (
	"encoding/json"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"slices"
	"strconv"
)

func activationPhysicalKey(c delivery.Connection, repository, ref string) string {
	authority, _ := runtimeGitOpsAuthority(c)
	return authority + "|" + repository + "|" + ref
}
func activationComponent(p *domain.DevActivationProof, id string) *domain.DevActivationComponent {
	for i := range p.Components {
		if p.Components[i].ChildID == id {
			return &p.Components[i]
		}
	}
	return nil
}
func activationGroup(p *domain.DevActivationProof, id string) *domain.DevActivationGroup {
	for i := range p.Groups {
		if p.Groups[i].ID == id {
			return &p.Groups[i]
		}
	}
	return nil
}
func freezeActivationGroups(p *domain.DevActivationProof, children []domain.ExternalOperation, baseline domain.ExternalOperation) error {
	if len(p.Components) == 0 || len(p.Components) > 16 {
		return invalid("candidate Dev group membership outside bounds")
	}
	keys := map[string]int{}
	for i := range p.Components {
		c := &p.Components[i]
		var source *domain.DevBaselineComponent
		for j := range baseline.DevBaseline.Components {
			b := &baseline.DevBaseline.Components[j]
			if b.ApplicationID == c.Artifact.ApplicationID {
				if source != nil {
					return invalid("ambiguous baseline component")
				}
				source = b
			}
		}
		if source == nil || source.ObservedGitOps == nil {
			return invalid("exact selected baseline component required")
		}
		c.Selector = source.GitOps
		c.Selector.ExpectedHeadSHA = ""
		c.Baseline = *source.ObservedGitOps
		if c.Selector.Repository != c.Snapshot.GitOpsRequest.Repository || c.Selector.Ref != c.Snapshot.GitOpsRequest.Ref || c.Selector.Path != c.Snapshot.GitOpsRequest.Path || c.Selector.ImageField != c.Snapshot.GitOpsRequest.ImageField || c.Selector.DigestField != c.Snapshot.GitOpsRequest.DigestField {
			return invalid("candidate Dev baseline mapping differs")
		}
		if _, e := runtimeGitOpsAuthority(c.Snapshot.GitOpsConnection); e != nil {
			return e
		}
		key := activationPhysicalKey(c.Snapshot.GitOpsConnection, c.Selector.Repository, c.Selector.Ref)
		n, ok := keys[key]
		if !ok {
			n = len(p.Groups)
			keys[key] = n
			p.Groups = append(p.Groups, domain.DevActivationGroup{ID: workerconfig.Fingerprint(key), Connection: c.Snapshot.GitOpsConnection, Repository: c.Selector.Repository, Ref: c.Selector.Ref})
		}
		g := &p.Groups[n]
		if !reflect.DeepEqual(g.Connection, c.Snapshot.GitOpsConnection) {
			return invalid("physical GitOps aliases require identical connection")
		}
		c.GroupID = g.ID
		g.MemberIDs = append(g.MemberIDs, c.ChildID)
		children[i].DevActivationGroupID = g.ID
	}
	for i := range p.Groups {
		g := &p.Groups[i]
		slices.Sort(g.MemberIDs)
		g.LeaderID = g.MemberIDs[0]
	}
	return activationGroupBounds(p, nil)
}
func activationGroupBounds(p *domain.DevActivationProof, operations []domain.ExternalOperation) error {
	if p == nil {
		return invalid("candidate Dev group proof missing")
	}
	b, e := json.Marshal(struct {
		Proof      *domain.DevActivationProof
		Operations []domain.ExternalOperation
	}{p, operations})
	// Reserve the existing bounded per-member failure detail/error budget too.
	if e != nil || len(b)+len(p.Components)*2000 > 256*1024 {
		return invalid("candidate Dev grouped authority exceeds bound")
	}
	return nil
}
func validateActivationGroups(st *domain.State, parent *domain.ExternalOperation) error {
	p := parent.DevActivation
	capture := operationByID(st, p.BaselineCaptureID)
	if capture == nil || capture.DevBaseline == nil {
		return invalid("group baseline capture missing")
	}
	for _, c := range p.Components {
		found := false
		for _, b := range capture.DevBaseline.Components {
			if b.ApplicationID == c.Artifact.ApplicationID && b.ObservedGitOps != nil {
				selector := b.GitOps
				selector.ExpectedHeadSHA = ""
				if !reflect.DeepEqual(c.Selector, selector) || !reflect.DeepEqual(c.Baseline, *b.ObservedGitOps) {
					return invalid("group original selector changed")
				}
				found = true
			}
		}
		if !found {
			return invalid("group original baseline missing")
		}
	}
	if len(p.Groups) == 0 || len(p.Groups) > 16 || len(p.Components) > 16 {
		return invalid("grouped candidate Dev capability required")
	}
	seen := map[string]bool{}
	physical := map[string]bool{}
	ops := []domain.ExternalOperation{}
	for _, g := range p.Groups {
		if _, e := runtimeGitOpsAuthority(g.Connection); e != nil {
			return e
		}
		key := activationPhysicalKey(g.Connection, g.Repository, g.Ref)
		if len(g.MemberIDs) == 0 || !slices.IsSorted(g.MemberIDs) || g.LeaderID != g.MemberIDs[0] || g.ID != workerconfig.Fingerprint(key) || physical[key] {
			return invalid("candidate Dev group identity changed")
		}
		physical[key] = true
		leader := operationByID(st, g.LeaderID)
		if leader == nil {
			return invalid("candidate Dev group leader missing")
		}
		for _, id := range g.MemberIDs {
			c := activationComponent(p, id)
			child := operationByID(st, id)
			if seen[id] || c == nil || child == nil || c.GroupID != g.ID || child.DevActivationGroupID != g.ID || !reflect.DeepEqual(c.Snapshot.GitOpsConnection, g.Connection) || c.Selector.Repository != g.Repository || c.Selector.Ref != g.Ref {
				return invalid("candidate Dev group membership changed")
			}
			if child.GitOpsBase != nil && (child.GitOpsBase.Content != "" || !fullSHA.MatchString(child.GitOpsBase.HeadSHA) || child.GitOpsBase.BlobSHA != c.Baseline.BlobSHA || child.GitOpsBase.ImageRepository != c.Baseline.ImageRepository || child.GitOpsBase.Digest != c.Baseline.Digest) {
				return invalid("candidate Dev original inspected base changed")
			}
			if child.GitOpsResult != nil {
				if leader.GitOpsResult == nil || child.Status != "SUCCEEDED" || child.GitOpsPR == nil || leader.GitOpsPR == nil || activationPRHash(*child.GitOpsPR) != activationPRHash(*leader.GitOpsPR) || child.GitOpsResult.CommitSHA != leader.GitOpsResult.CommitSHA || child.GitOpsResult.URL != child.GitOpsPR.URL || !fullSHA.MatchString(child.GitOpsResult.CommitSHA) || !fullSHA.MatchString(child.GitOpsResult.BlobSHA) {
					return invalid("candidate Dev atomic group result changed")
				}
			}
			if leader.GitOpsResult != nil && child.GitOpsResult == nil {
				return invalid("candidate Dev partial group result")
			}
			seen[id] = true
			if child.ID != g.LeaderID && (child.DevActivationBatch != nil || child.DevActivationIntentHash != "") {
				return invalid("follower gained proposal authority")
			}
			ops = append(ops, domain.ExternalOperation{DevActivationBatch: child.DevActivationBatch, GitOpsBase: child.GitOpsBase, GitOpsPR: child.GitOpsPR, GitOpsResult: child.GitOpsResult})
		}
		if leader.DevActivationBatch != nil {
			b := leader.DevActivationBatch
			raw, e := json.Marshal(b)
			if e != nil || len(raw) > 64*1024 || b.Repository != g.Repository || b.Ref != g.Ref || b.OperationID != g.LeaderID || b.Message != "release-control: "+g.LeaderID || !fullSHA.MatchString(b.ExpectedHeadSHA) || len(b.Changes) != len(g.MemberIDs) || leader.DevActivationIntentHash != workerconfig.Fingerprint(b) {
				return invalid("candidate Dev prepared group intent changed")
			}
			for i, id := range g.MemberIDs {
				c := activationComponent(p, id)
				v := b.Changes[i]
				if v.ID != id || !reflect.DeepEqual(v.Selector, c.Selector) || v.ExpectedBlobSHA != c.Baseline.BlobSHA || v.ImageRepository != c.Artifact.ImageRepository || v.Digest != c.Artifact.Digest {
					return invalid("candidate Dev prepared member intent changed")
				}
			}
		} else if leader.DevActivationIntentHash != "" || leader.Phase == "APPLY_UNCERTAIN" || leader.Phase == "WAIT_GITOPS_PR" {
			return invalid("candidate Dev prepared group intent missing")
		}
	}
	if len(seen) != len(p.Components) {
		return invalid("candidate Dev incomplete grouped membership")
	}
	return activationGroupBounds(p, ops)
}

// Reject overlapping aliases and oversized membership before provider preflight.
func activationDestinationsAllowed(st *domain.State, candidate *domain.ExternalOperation, environment, product string) error {
	if len(candidate.ChildIDs) == 0 || len(candidate.ChildIDs) > 16 {
		return invalid("candidate Dev requires one to sixteen selected members")
	}
	scopes := map[string]delivery.Connection{}
	seenChildren, seenApplications := map[string]bool{}, map[string]bool{}
	for _, id := range candidate.ChildIDs {
		origin := operationByID(st, id)
		if origin == nil || origin.ParentID != candidate.ID || origin.ProductID != product || origin.ApplicationID == "" {
			return invalid("candidate member missing or foreign")
		}
		if seenChildren[id] || seenApplications[origin.ApplicationID] {
			return invalid("duplicate candidate Dev original child or application")
		}
		seenChildren[id], seenApplications[origin.ApplicationID] = true, true
		target := environmentBinding(st, environment, origin.ApplicationID)
		if target == nil {
			return invalid("candidate Dev mapping missing")
		}
		repo := repositoryBinding(st, target.RepositoryID)
		if repo == nil {
			return invalid("candidate Dev repository missing")
		}
		conn, e := scopedConnection(st, target.ConnectionID, product)
		if e != nil {
			return e
		}
		if _, e := runtimeGitOpsAuthority(conn.Config); e != nil {
			return e
		}
		locator := repositoryLocator(st, *repo)
		numeric, e := strconv.ParseInt(locator, 10, 64)
		if e != nil || numeric <= 0 || strconv.FormatInt(numeric, 10) != locator {
			return invalid("candidate Dev numeric repository required")
		}
		key := activationPhysicalKey(conn.Config, locator, target.Ref)
		if old, ok := scopes[key]; ok && !reflect.DeepEqual(old, conn.Config) {
			return invalid("physical GitOps aliases require identical connection")
		}
		scopes[key] = conn.Config
	}
	return nil
}

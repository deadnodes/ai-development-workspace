package application

import (
	"errors"
	"strings"
	"testing"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func TestProductAgentSkillsStoreMatrix(t *testing.T) {
	privateReviewStores(t, func(t *testing.T, store Store) {
		work := ownerWorkReviewOnStore(t, store)
		admin := userCtx("admin", "human", "", "admin")
		privateReviewCreate(t, work.service, work.owner, "secret", "private")
		privateReviewCreate(t, work.service, work.owner, "unsafe.product", "shared")
		input := map[string]any{"name": "orchestrator-role", "description": "Review the execution group.", "body": "Read this first.", "files": map[string]string{"scripts/check.sh": "echo ready", "integration_id": "opaque text"}}
		for _, caller := range []struct {
			kind string
			want error
		}{{"shared", auth.ErrForbidden}, {"private", ErrNotFound}} {
			product := "p"
			if caller.kind == "private" {
				product = "secret"
			}
			_, err := work.apply(work.bob, "upsert_agent_skill", product, "", "", "", input)
			if !errors.Is(err, caller.want) {
				t.Fatalf("%s unauthorized write: %v", caller.kind, err)
			}
		}
		if _, err := work.apply(work.agent, "upsert_agent_skill", "p", "", "", "", input); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("owner-linked agent published: %v", err)
		}
		private := work.run(work.owner, "upsert_agent_skill", "secret", "", "", "", input).(domain.ProductAgentSkill)
		if private.Version != 1 || private.UpdatedBy != "alice" || private.BodySHA256 != skillDigest([]byte(private.Body)) || private.FilesSHA256["scripts/check.sh"] != skillDigest([]byte("echo ready")) {
			t.Fatalf("bad private skill: %+v", private)
		}
		if _, err := work.service.AgentSkill(work.bob, "secret", "rcp-execution"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("private skill scope disclosed: %v", err)
		}
		if _, err := work.service.AgentKit(work.bob, "secret"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("private kit scope disclosed: %v", err)
		}
		if _, err := work.service.AgentSkill(work.agent, "p", private.Name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("skill leaked across products: %v", err)
		}
		first := work.run(admin, "upsert_agent_skill", "p", "", "", "", input).(domain.ProductAgentSkill)
		same := work.run(work.owner, "upsert_agent_skill", "p", "", "", "", input).(domain.ProductAgentSkill)
		if first.ID != same.ID || same.Version != 1 || same.UpdatedBy != "admin" || same.SHA256 != private.SHA256 {
			t.Fatalf("identical push changed skill: %+v -> %+v", first, same)
		}
		input["body"] = "Updated instructions."
		updated := work.run(work.owner, "upsert_agent_skill", "p", "", "", "", input).(domain.ProductAgentSkill)
		loaded, err := work.service.AgentSkill(work.agent, "p", updated.Name)
		if err != nil || updated.ID != first.ID || updated.UpdatedBy != "alice" || updated.Version != 2 || first.SHA256 == updated.SHA256 {
			t.Fatalf("bad update: %+v %v", updated, err)
		}
		if got := loaded.(domain.ProductAgentSkill); got.Body != input["body"] || got.Files["scripts/check.sh"] != "echo ready" || got.SHA256 != updated.SHA256 {
			t.Fatalf("skill readback: %+v", got)
		}
		for _, test := range []struct {
			name, body, err string
			files           map[string]string
		}{
			{"rcp-execution", "body", "cannot be shadowed", nil},
			{"Invalid", "body", "skill name", nil},
			{"bad-body", strings.Repeat("b", domain.MaxAgentSkillBodyBytes+1), "64 KiB", nil},
			{"bad-files", "body", "256 KiB", map[string]string{"first": strings.Repeat("f", domain.MaxAgentSkillFilesBytes), "second": "x"}},
			{"bad-path", "body", "safe relative path", map[string]string{"../file": "x"}},
		} {
			_, err := work.apply(work.owner, "upsert_agent_skill", "p", "", "", "", map[string]any{"name": test.name, "description": "Test description", "body": test.body, "files": test.files})
			expectWorkError(t, err, test.err)
		}
		roles := map[string]any{"orchestrator_skill": updated.Name, "worker_skill": "rcp-execution"}
		if _, err := work.apply(admin, "set_product_agent_roles", "p", "", "", "", roles); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("non-owner admin changed roles: %v", err)
		}
		_, err = work.apply(work.owner, "set_product_agent_roles", "unsafe.product", "", "", "", roles)
		expectWorkError(t, err, "product ID is not valid")
		_, err = work.apply(work.owner, "set_product_agent_roles", "p", "", "", "", map[string]any{"orchestrator_skill": "missing-role", "worker_skill": "rcp-execution"})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown role skill accepted: %v", err)
		}
		work.run(work.owner, "set_product_agent_roles", "p", "", "", "", roles)
		kit, err := work.service.AgentKit(work.agent, "p")
		if err != nil || kit.AgentRoles == nil || kit.AgentRoles.OrchestratorSkill != updated.Name || len(kit.Skills) != 4 || kit.Skills[3].Description != updated.Description || !strings.Contains(kit.Skills[3].When, "orchestrator") || !strings.Contains(kit.Skills[2].When, "worker") {
			t.Fatalf("bad scoped agent kit: %+v %v", kit, err)
		}
		context, err := work.service.ProjectContextSummary(work.agent, "p")
		if err != nil || context.(map[string]any)["product"].(map[string]any)["agent_roles"] != *kit.AgentRoles {
			t.Fatalf("context missing roles: %v %v", context, err)
		}
		name := map[string]any{"name": updated.Name}
		_, err = work.apply(work.owner, "delete_agent_skill", "p", "", "", "", name)
		expectWorkError(t, err, "assigned to a product role")
		work.run(work.owner, "set_product_agent_roles", "p", "", "", "", map[string]any{"orchestrator_skill": "rcp-handoff", "worker_skill": "rcp-execution"})
		work.run(admin, "delete_agent_skill", "p", "", "", "", name)
		if _, err := work.service.AgentSkill(work.agent, "p", updated.Name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted skill remains readable: %v", err)
		}
	})
}

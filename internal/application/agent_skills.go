package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"releasecontrol/internal/agentguide"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func agentSkillIndex(st *domain.State, product, name string) int {
	return slices.IndexFunc(st.ProductAgentSkills, func(skill domain.ProductAgentSkill) bool {
		return skill.ProductID == product && skill.Name == name
	})
}

func skillDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func changeProductAgentSkills(ctx context.Context, st *domain.State, command domain.Command, metadata domain.Meta) (any, error) {
	product := productByID(*st, command.ProductID)
	if product == nil || !productVisibleTo(ctx, product) {
		return nil, missing("product", command.ProductID)
	}
	if principal, ok := auth.FromContext(ctx); ok && !instanceAdmin(principal) && (principal.Kind != "human" || principal.ID != product.Owner) {
		return nil, auth.ErrForbidden
	}
	if command.ID != "" || command.FeatureID != "" || command.IntegrationID != "" || command.GateID != "" || command.CheckID != "" || command.ResultID != "" {
		return nil, invalid("agent skills and roles take only a product scope")
	}
	switch command.Action {
	case "set_product_agent_roles":
		if !domain.ValidAgentRoleProductID(product.ID) {
			return nil, invalid("product ID is not valid for agent role instructions")
		}
		var roles domain.AgentRoles
		if err := decodeWritable(command.Data, &roles, "orchestrator_skill", "worker_skill"); err != nil {
			return nil, err
		}
		for _, name := range []string{roles.OrchestratorSkill, roles.WorkerSkill} {
			if !domain.ValidAgentSkillName(name) {
				return nil, invalid("both roles must name an existing skill")
			}
			if !agentguide.BuiltInSkill(name) && agentSkillIndex(st, product.ID, name) < 0 {
				return nil, missing("agent skill", name)
			}
		}
		product.AgentRoles = &roles
		return roles, nil
	case "delete_agent_skill":
		var input struct{ Name string `json:"name"` }
		if err := decodeWritable(command.Data, &input, "name"); err != nil {
			return nil, err
		}
		if !domain.ValidAgentSkillName(input.Name) || agentguide.BuiltInSkill(input.Name) {
			return nil, invalid("expected a valid product skill name, not a built-in")
		}
		index := agentSkillIndex(st, product.ID, input.Name)
		if index < 0 {
			return nil, missing("agent skill", input.Name)
		}
		if roles := product.AgentRoles; roles != nil && (roles.OrchestratorSkill == input.Name || roles.WorkerSkill == input.Name) {
			return nil, invalid("skill is assigned to a product role; change the role before deleting")
		}
		st.ProductAgentSkills = slices.Delete(st.ProductAgentSkills, index, index+1)
		return map[string]string{"name": input.Name}, nil
	default:
		var input domain.AgentSkillInput
		if err := decodeWritable(command.Data, &input, "name", "description", "body", "files"); err != nil {
			return nil, err
		}
		if err := input.Validate(); err != nil {
			return nil, invalid("%s", err)
		}
		if agentguide.BuiltInSkill(input.Name) {
			return nil, invalid("built-in skill names cannot be shadowed")
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		skill := domain.ProductAgentSkill{Meta: metadata, AgentSkillInput: input, Version: 1, SHA256: skillDigest(encoded), BodySHA256: skillDigest([]byte(input.Body)), UpdatedBy: command.Actor}
		if len(input.Files) > 0 {
			skill.FilesSHA256 = make(map[string]string, len(input.Files))
			for name, text := range input.Files {
				skill.FilesSHA256[name] = skillDigest([]byte(text))
			}
		}
		if index := agentSkillIndex(st, product.ID, input.Name); index >= 0 {
			old := st.ProductAgentSkills[index]
			if old.SHA256 == skill.SHA256 {
				return old, nil
			}
			skill.ID, skill.CreatedAt, skill.Version = old.ID, old.CreatedAt, old.Version+1
			st.ProductAgentSkills[index] = skill
		} else {
			st.ProductAgentSkills = append(st.ProductAgentSkills, skill)
		}
		return skill, nil
	}
}

func (s *Service) queryAgentGuide(ctx context.Context, query, product, name string) (any, error) {
	var result any
	err := s.view(ctx, func(st *domain.State) error {
		var roles *domain.AgentRoles
		if product != "" {
			record := productByID(*st, product)
			if record == nil || !productVisibleTo(ctx, record) {
				return missing("product", product)
			}
			roles = record.AgentRoles
		}
		if query == "get_agent_skill" {
			if skill, exists := agentguide.Lookup(name); exists {
				result = skill
				return nil
			}
			if index := agentSkillIndex(st, product, name); product != "" && index >= 0 {
				result = st.ProductAgentSkills[index]
				return nil
			}
			return missing("agent skill", name)
		}
		kit := agentguide.Get()
		kit.AgentRoles = roles
		for _, skill := range st.ProductAgentSkills {
			if product == "" || skill.ProductID != product {
				continue
			}
			when := "Load when the workflow calls for this skill."
			if roles != nil {
				var assigned []string
				if skill.Name == roles.OrchestratorSkill {
					assigned = append(assigned, "orchestrator")
				}
				if skill.Name == roles.WorkerSkill {
					assigned = append(assigned, "worker")
				}
				if len(assigned) != 0 {
					when = "Load before acting as " + strings.Join(assigned, " or ") + "."
				}
			}
			kit.Skills = append(kit.Skills, agentguide.SkillSummary{Name: skill.Name, Description: skill.Description, When: when, Version: skill.Version, SHA256: skill.SHA256})
		}
		result = kit
		return nil
	})
	return result, err
}

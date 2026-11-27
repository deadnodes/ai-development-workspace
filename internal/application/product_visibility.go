package application

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func (s *Service) executeProductMembership(ctx context.Context, c domain.Command) (any, error) {
	for k := range c.Data {
		want := "visibility"
		if c.Action != "set_product_visibility" {
			want = "user_id"
		}
		if k != want {
			return nil, invalid("unknown field %s", k)
		}
	}
	var out domain.Product
	err := s.store.Update(ctx, func(st *domain.State) error {
		p := productByID(*st, c.ProductID)
		if p == nil {
			return missing("product", c.ProductID)
		}
		if !productVisibleTo(ctx, p) {
			return missing("product", c.ProductID)
		}
		principal, authenticated := auth.FromContext(ctx)
		bootstrap := authenticated && principal.Kind == "service" && principal.ID == "service/bootstrap"
		user, _ := auth.PrivateProductViewer(ctx)
		unownedShared := p.Visibility != "private" && (p.Owner == "" || p.Owner == "local")
		claimingShared := c.Action == "set_product_visibility" && unownedShared && user != ""
		localAdmin := auth.IsLocalAdmin(ctx)
		if localAdmin {
			user = "local"
		}
		if !bootstrap && !localAdmin && (user == "" || (p.Owner != user && !claimingShared)) {
			return auth.ErrForbidden
		}
		if c.Action == "set_product_visibility" {
			visibility, ok := c.Data["visibility"].(string)
			if !ok || (visibility != "private" && visibility != "shared") {
				return invalid("visibility must be shared or private")
			}
			if claimingShared || (p.Owner == "" && visibility == "private") {
				if user == "" && !bootstrap {
					return fmt.Errorf("%w: PRIVATE_PRODUCT_REQUIRES_USER", auth.ErrForbidden)
				}
				p.Owner = user
			}
			p.Visibility = visibility
		} else {
			memberID, ok := c.Data["user_id"].(string)
			memberID = strings.TrimSpace(memberID)
			if !ok || memberID == "" {
				return invalid("user_id required")
			}
			if memberID == p.Owner && c.Action == "remove_product_member" {
				return invalid("product owner cannot be removed")
			}
			if c.Action == "add_product_member" {
				if !slices.Contains(p.Members, memberID) {
					p.Members = append(p.Members, memberID)
					slices.Sort(p.Members)
				}
			} else {
				p.Members = slices.DeleteFunc(p.Members, func(v string) bool { return v == memberID })
			}
		}
		p.Actor, p.UpdatedAt = c.Actor, nowUTC()
		out = *p
		st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: p.UpdatedAt, EntityID: p.ID, ProductID: p.ID, Data: c})
		return nil
	})
	return out, err
}

func nowUTC() time.Time { return monotonicNow() }

func productIDRetired(st *domain.State, productID string) bool {
	for _, event := range st.Events {
		if event.Action == "delete_product" && event.ProductID == productID {
			return true
		}
	}
	return false
}

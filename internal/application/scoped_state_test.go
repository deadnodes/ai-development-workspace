package application

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func scopedStateJSONReference(ctx context.Context, st domain.State) (domain.State, error) {
	if err := authorize(ctx, "read", ""); err != nil {
		return domain.State{}, err
	}
	p, ok := auth.FromContext(ctx)
	if !ok || instanceAdmin(p) {
		return st, nil
	}
	rows, err := stateRows(st)
	if err != nil {
		return domain.State{}, err
	}
	for kind, group := range rows {
		filtered := []map[string]any{}
		for _, row := range group {
			if allowedProduct(p, rowProduct(kind, row)) {
				filtered = append(filtered, row)
			}
		}
		rows[kind] = filtered
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return domain.State{}, err
	}
	var out domain.State
	err = json.Unmarshal(b, &out)
	return out, err
}

func setTaggedString(v reflect.Value, tagName, value string) bool {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return false
	}
	found := false
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		sf := v.Type().Field(i)
		tag := strings.Split(sf.Tag.Get("json"), ",")[0]
		if tag == tagName && f.Kind() == reflect.String && f.CanSet() {
			f.SetString(value)
			found = true
		}
		if sf.Anonymous && f.Kind() == reflect.Struct {
			found = setTaggedString(f, tagName, value) || found
		}
	}
	return found
}
func scopedFixture() domain.State {
	st := domain.EmptyState()
	typ := reflect.TypeOf(st)
	val := reflect.ValueOf(&st).Elem()
	for i := 0; i < typ.NumField(); i++ {
		f := val.Field(i)
		if f.Kind() != reflect.Slice {
			continue
		}
		elem := f.Type().Elem()
		xs := reflect.MakeSlice(f.Type(), 0, 3)
		for _, p := range []string{"p", "q", ""} {
			item := reflect.New(elem).Elem()
			if typ.Field(i).Name == "Products" {
				setTaggedString(item, "id", p)
			} else {
				setTaggedString(item, "product_id", p)
			}
			xs = reflect.Append(xs, item)
		}
		f.Set(xs)
	}
	return st
}
func TestScopedStateParity(t *testing.T) {
	st := scopedFixture()
	principals := []*auth.Principal{{Products: []string{"p"}, Permissions: []string{"read"}}, {Products: []string{"*"}, Permissions: []string{"read"}}, {Products: []string{}, Permissions: []string{"read"}}, {Products: []string{"p", "q"}, Permissions: []string{"read"}}, {ID: "human/admin", Kind: "human", Role: "admin", Products: []string{"*"}, Permissions: []string{"admin"}}, nil}
	for _, p := range principals {
		ctx := context.Background()
		if p != nil {
			ctx = auth.WithPrincipal(ctx, *p)
		}
		got, e := scopedState(ctx, st)
		if e != nil {
			t.Fatal(e)
		}
		want, e := scopedStateJSONReference(ctx, st)
		if e != nil {
			t.Fatal(e)
		}
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		if string(a) != string(b) {
			t.Fatalf("principal %#v parity mismatch", p)
		}
	}
}
func TestScopedStateDropsCollectionWithoutProductID(t *testing.T) {
	type row struct {
		ID string `json:"id"`
	}
	rows := []row{{ID: "global"}}
	value := reflect.ValueOf(rows)
	_, ok := stateRowProduct(value.Index(0), false)
	if ok {
		t.Fatal("collection without product_id accepted")
	}
	filtered := slices.DeleteFunc(rows, func(r row) bool { v, found := stateRowProduct(reflect.ValueOf(r), false); return !found || v == "" })
	if len(filtered) != 0 {
		t.Fatal("global row was retained")
	}
}

func TestIntegrationContextJSONMatchesTwoReadAssembly(t *testing.T) {
	s, _ := fixture(t)
	// Preserve the old query's first read and second Resume read as the reference.
	st, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	freshness, err := s.runtimeFreshness(context.Background(), &st)
	if err != nil {
		t.Fatal(err)
	}
	request := *s
	request.runtimeFreshnessSnapshot = freshness
	featureContext, err := request.Resume(context.Background(), "f")
	if err != nil {
		t.Fatal(err)
	}
	in := integration(&st, "i")
	if in == nil {
		t.Fatal("fixture integration missing")
	}
	expected := map[string]any{"integration": in, "feature_context": featureContext}
	addDeliveryContext(expected, st, in.FeatureID, in.ID)
	expected["external_context"] = externalContext(&st, in.FeatureID, in.ID)
	got, err := s.Query(context.Background(), "get_integration_context", "i")
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(expected)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("integration context changed\nwant %s\n got %s", wantJSON, gotJSON)
	}
}

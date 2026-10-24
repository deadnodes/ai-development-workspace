package transport

import "testing"

func TestStatePageStrictArguments(t *testing.T) {
	for _, s := range []string{`null`, `[]`, `{"Product_ID":"p"}`, `{"product_id":"p","product_id":"q"}`, `{"product_id":null}`, `{"feature_id":""}`, `{"limit":0}`, `{"limit":1.0}`, `{"limit":null}`, `{"limit":101}`, `{"cursor":null}`, `{"unknown":"a"}`, `{} {}`} {
		if _, e := decodeStatePageArguments([]byte(s)); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	if in, e := decodeStatePageArguments([]byte(`{"product_id":"p","feature_id":"f","limit":25}`)); e != nil || in.FeatureID != "f" {
		t.Fatal(in, e)
	}
}

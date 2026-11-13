package engine

// compat_v002_test.go: guards for behaviour v0.0.2 users rely on.

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// SessionRegistry is an interface applications implement (e.g. test doubles):
// adding a method breaks them. New session operations go on Engine instead.
func TestSessionRegistryMethodSetUnchanged(t *testing.T) {
	want := []string{"Ancestry", "BranchTree", "Create", "Discard", "ForcePurge", "Fork", "Get", "GetHeader",
		"GetIncludingDeleted", "List", "Pin", "Purge", "StateAt", "Steps", "Unpin", "Update"}
	typ := reflect.TypeOf((*SessionRegistry)(nil)).Elem()
	var got []string
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("SessionRegistry methods = %v, want the v0.0.2 set %v", got, want)
	}
}

// v0.0.2's Estimate never failed: it took sample input alone and ignored the
// agent. It must still answer such calls, flagging the price as unconfigured.
func TestEstimateAcceptsV002Calls(t *testing.T) {
	ctx := context.Background()
	e, _ := reproEngine(t, "estcompat", map[string]Generator{"g": okGen{}}, PollerConfig{})
	for name, req := range map[string]EstimateRequest{
		"input only":    {Input: "roughly four hundred characters of prompt text, give or take", Modal: ModalityText},
		"unknown agent": {AgentID: uuid.New(), Input: "hello there", Modal: ModalityText},
	} {
		est, err := e.Cost().Estimate(ctx, req)
		if err != nil {
			t.Fatalf("%s: err = %v", name, err)
		}
		if est.PricingConfigured || est.OutputTokensEstimated != 512 || est.USDHigh <= est.USDLow {
			t.Fatalf("%s: estimate = %+v", name, est)
		}
	}
}

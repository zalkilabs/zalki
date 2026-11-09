package bus_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginInboxKeepsEightNativeAddressForms(t *testing.T) {
	for _, form := range []string{"plain", "workspace-label", "plugin-label", "plugin-uppercase", "canonical-plugin", "wrong-kind", "kind-alias", "ambiguous"} {
		t.Run(form, func(t *testing.T) {
			checkInboxAddressForm(t, form)
		})
	}
}

func checkInboxAddressForm(t *testing.T, form string) {
	t.Helper()

	x := newInboxPublicFixture(t)
	to := setInboxAddressForm(t, x, form)
	x.service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		t.Error("address selection invoked a native transport")
		return model.NewReceipt(model.StatusSubmitted)
	}
	result, err := x.service.Send(context.Background(), bus.SendRequest{Actor: x.author.ID, Capability: x.author.Key, To: to, MessageID: newUUID(t), Body: "address compatibility fixture"})
	switch form {
	case "ambiguous":
		if model.ErrorCodeOf(err) != model.CodeConflict {
			t.Fatal("native ambiguity changed", err)
		}
	case "wrong-kind":
		if err == nil {
			t.Fatal("native kind qualifier ignored")
		}
	case "canonical-plugin":
		if err != nil || !reflect.DeepEqual([]any{result.To, result.TargetGeneration, result.Status, result.AcknowledgedAt}, []any{x.to, x.installation, model.StatusSubmitted, ""}) {
			t.Fatal("canonical plugin namespace routed as a native alias", err)
		}
	default:
		if err != nil || result.To != x.third.ID || result.RequestedTo != to || result.Status != model.StatusPendingActivity || result.AcknowledgedAt != "" {
			t.Fatal("native address behavior changed", form, err)
		}
	}

}

func setInboxAddressForm(t *testing.T, x inboxPublicFixture, form string) string {
	t.Helper()
	to, name := "native-target", "native-target"
	switch form {
	case "workspace-label":
		to, name = "workspace-label", "workspace-label"
	case "plugin-label":
		to, name = "plugin:label", "plugin:label"
	case "plugin-uppercase":
		to, name = "plugin:ABCDEF00-0000-4000-8000-000000000001", "plugin:ABCDEF00-0000-4000-8000-000000000001"
	case "canonical-plugin":
		to, name = x.to, x.to
	case "wrong-kind":
		to, name = "codex:alias", "codex:alias"
	case "kind-alias":
		to, name = "cursor:alias", "alias"
	case "ambiguous":
		to, name = "ambiguous", "ambiguous"
	}
	target := readJSONFile(t, sessionFile(x.service.Store, x.third))
	target["name"], target["name_source"] = name, "manual"
	writeJSONFile(t, sessionFile(x.service.Store, x.third), target)
	if form == "ambiguous" {
		other := readJSONFile(t, sessionFile(x.service.Store, x.other))
		other["name"], other["name_source"] = name, "manual"
		writeJSONFile(t, sessionFile(x.service.Store, x.other), other)
	}

	return to
}

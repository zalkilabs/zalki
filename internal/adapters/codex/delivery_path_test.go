package codex

import (
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// The receipt names the route the adapter actually took, for the transport log.
// It does not change a status, an output text, or the fallback decision.
func TestCodexReceiptNamesTheDeliveryPath(t *testing.T) {
	for _, test := range []struct {
		mode  string
		allow bool
		want  string
	}{
		{"idle", true, deliveryPathAppServer},
		{"busy", true, deliveryPathAppServer},
		{"approval", true, deliveryPathAppServer},
		{"dropped", true, deliveryPathAppServer},
		{"method-missing", true, deliveryPathNativeQueue},
		{"not-loaded", true, deliveryPathNativeQueue},
		{"method-missing", false, ""},
	} {
		name := test.mode + map[bool]string{false: "/queue-off", true: "/queue-on"}[test.allow]
		t.Run(name, func(t *testing.T) {
			root, listener := listenCodexControl(t)
			defer listener.Close()
			thread := model.NewUUID()
			queueSpy(t)
			serveFakeCodex(t, listener, fakeCodex{Home: root, Thread: thread, Mode: test.mode})
			msg := model.Message{ID: model.NewUUID(), From: "cursor:" + model.NewUUID(), FromLabel: "Cursor", FromName: "Review", To: "codex:" + thread, Body: "path fixture", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			receipt := New(test.allow, "fixture.json").deliverCodex(t.Context(), msg, model.Session{Kind: "codex", ID: msg.To, NativeID: thread, NativeConfigDir: root})
			if receipt.Path() != test.want {
				t.Fatalf("path = %q, want %q (status %s)", receipt.Path(), test.want, receipt.Status)
			}
		})
	}
}

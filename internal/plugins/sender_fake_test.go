package plugins

import (
	"context"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

var _ Sender = (*bus.Service)(nil)

type fakeSender struct {
	got []bus.PluginSendRequest
	err error
}

func (f *fakeSender) SendFromPlugin(_ context.Context, r bus.PluginSendRequest) (model.Message, error) {
	f.got = append(f.got, r)
	return model.Message{ID: "m-1"}, f.err
}

func TestSendMessageUsesSenderWithBindingAndIdempotencyKey(t *testing.T) {
	fake := &fakeSender{}
	h := &v2Host{ctx: t.Context(), service: fake, binding: &pluginfeed.PluginFeedStatus{ID: "inst-1", Name: "feed-a"}}
	var result v2Result
	h.sendMessage(&v2Command{IdempotencyKey: "c1", Send: &v2Send{ReplyTo: "r", To: "t", Body: "hello"}}, &result)
	want := bus.PluginSendRequest{Installation: "inst-1", Name: "feed-a", ClientMessageID: "c1", ReplyTo: "r", To: "t", Body: "hello"}
	if len(fake.got) != 1 || fake.got[0] != want {
		t.Fatalf("sender got %+v, want one %+v", fake.got, want)
	}
	if result.Code != "ok" {
		t.Fatalf("code %q", result.Code)
	}
}

func TestSendMessageMapsSenderErrorToCode(t *testing.T) {
	fake := &fakeSender{err: model.NewError(model.CodeUnauthorized, "no")}
	h := &v2Host{ctx: t.Context(), service: fake, binding: &pluginfeed.PluginFeedStatus{ID: "i", Name: "n"}}
	var result v2Result
	h.sendMessage(&v2Command{IdempotencyKey: "c", Send: &v2Send{To: "t", Body: "b"}}, &result)
	if result.Code != "unauthorized" || result.Result != nil {
		t.Fatalf("result %+v", result)
	}
}

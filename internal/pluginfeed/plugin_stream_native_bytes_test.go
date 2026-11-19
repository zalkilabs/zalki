package pluginfeed

import (
	"reflect"
	"strings"
	"testing"
)

func TestStreamFeedInspectionAndAckPreserveAllNativeBytes(t *testing.T) {
	feed, launch, _, _, offer := streamPendingMessageFixture(t)
	before := nativeFeedImage(t, feed)
	owner := readCommitFile(t, feed.pluginPath())
	streamComplete(t, feed, launch, offer)
	for range 3 {
		streamInspect(t, feed, launch)
	}
	if !reflect.DeepEqual(before, nativeFeedImage(t, feed)) {
		t.Fatal("F_FEED_READER_AND_ACK_NEVER_REWRITE_NATIVE_BYTES")
	}
	if !reflect.DeepEqual(owner, readCommitFile(t, feed.pluginPath())) {
		t.Fatal("F_FEED_MEMBERSHIP_AND_COUNTERS_NEVER_REWRITE_BINDING")
	}
}

func nativeFeedImage(t *testing.T, feed PluginFeed) map[string]string {
	t.Helper()
	image := journalSnapshot(t, feed.store)
	for name := range image {
		if strings.HasPrefix(name, "plugins/") {
			delete(image, name)
		}
	}
	return image
}

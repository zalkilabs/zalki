package pluginfeed

import (
	"context"
	"sync"
)

// Older operation fixtures model one continuing host per installation. Restart
// controls construct a fresh feed and call the public API directly. Every
// fixture offer still uses the real prepare/preflight and durable ACK paths.
var fixtureFeedRuns = struct {
	sync.Mutex
	runs map[string]*streamRuntime
}{runs: map[string]*streamRuntime{}}

func fixtureFeedRun(feed PluginFeed) PluginFeed {
	key := feed.root + "\x00" + feed.name
	fixtureFeedRuns.Lock()
	defer fixtureFeedRuns.Unlock()
	if runtime := fixtureFeedRuns.runs[key]; runtime != nil {
		feed.stream = runtime
	} else {
		freezeStreamWork(feed)
		fixtureFeedRuns.runs[key] = feed.stream
	}
	return feed
}

func inspectFixtureStream(ctx context.Context, feed PluginFeed, launch *PluginFeedStatus) (PluginDelivery, error) {
	return feed.NextPluginFeed(ctx, launch, "", 0)
}

func testFeedNext(feed PluginFeed, launch *PluginFeedStatus) (PluginDelivery, error) {
	feed = fixtureFeedRun(feed)
	bound := *launch
	bound.FeedFeatures.Feed = true
	delivery, err := inspectFixtureStream(context.Background(), feed, &bound)
	if err != nil || delivery.ID == "" {
		return delivery, err
	}
	id := delivery.ID
	for i := range delivery.Frames {
		delivery, err = feed.NextPluginFeed(context.Background(), &bound, id, i+1)
		if err != nil || delivery.ID != id {
			return delivery, err
		}
	}
	return delivery, nil
}

func testFeedAck(feed PluginFeed, delivery PluginDelivery) error {
	status, err := feed.PluginStatus()
	if err != nil {
		return err
	}
	status.FeedFeatures.Feed = true
	_, err = feed.AckPluginFeed(&status, delivery.ID)
	return err
}

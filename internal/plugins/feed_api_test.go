package plugins

import (
	"context"
	"sync"

	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

var fixtureHostFeeds = struct {
	sync.Mutex
	feeds map[string]pluginfeed.PluginFeed
}{feeds: map[string]pluginfeed.PluginFeed{}}

func testFeedNext(feed pluginfeed.PluginFeed, launch *pluginfeed.PluginFeedStatus) (pluginfeed.PluginDelivery, error) {
	bound := *launch
	bound.FeedFeatures.Feed = true
	fixtureHostFeeds.Lock()
	if existing, ok := fixtureHostFeeds.feeds[bound.DataDir]; ok {
		feed = existing
	} else {
		fixtureHostFeeds.feeds[bound.DataDir] = feed
	}
	fixtureHostFeeds.Unlock()
	delivery, err := feed.NextPluginFeed(context.Background(), &bound, "", 0)
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

func testFeedAck(feed pluginfeed.PluginFeed, delivery pluginfeed.PluginDelivery) error {
	status, err := feed.PluginStatus()
	if err != nil {
		return err
	}
	status.FeedFeatures.Feed = true
	_, err = feed.AckPluginFeed(&status, delivery.ID)
	return err
}

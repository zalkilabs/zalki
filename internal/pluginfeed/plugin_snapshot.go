package pluginfeed

// PluginSnapshot reads and strictly validates the current record like
// PluginStatus, but without the Store lock: the record is only ever replaced
// atomically, so one read sees one whole record. It never writes. The
// supervisor polls it so a held journal lock cannot stall supervision.
func (s PluginFeed) PluginSnapshot() (PluginFeedStatus, error) {
	if err := s.checkName(); err != nil {
		return PluginFeedStatus{}, err
	}
	p, _, err := s.loadPlugin()
	return s.statusOf(p), err
}

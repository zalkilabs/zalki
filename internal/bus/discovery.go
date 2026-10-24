package bus

// AgentInfo is the discovery view. Journal schemas, native IDs, adapter state,
// credential hashes and endpoint details are not part of the MCP contract.
type AgentInfo struct {
	ID       string              `json:"id"`
	Kind     string              `json:"kind"`
	Name     string              `json:"name,omitempty"`
	CWD      string              `json:"cwd"`
	State    SessionState        `json:"state"`
	Liveness string              `json:"liveness"`
	Receive  ReceiveCapabilities `json:"receive"`
}

func (s Store) Discover() ([]AgentInfo, error) {
	sessions, err := s.List()
	if err != nil {
		return nil, err
	}
	info := make([]AgentInfo, 0, len(sessions))
	for _, v := range sessions {
		info = append(info, AgentInfo{ID: v.ID, Kind: v.Kind, Name: v.Name, CWD: v.CWD, State: v.State, Liveness: v.Liveness, Receive: v.Receive})
	}
	return info, nil
}

package bus

// runtimeRecord owns the file header without adding schema to RuntimeInfo's
// public diagnostic projection.
type runtimeRecord struct {
	Schema int `json:"schema"`
	RuntimeInfo
}

func readRuntimeRecord(path string, target *RuntimeInfo) error {
	var record runtimeRecord
	if err := readRecord(path, &record, &record.Schema); err != nil {
		return err
	}
	*target = record.RuntimeInfo
	return nil
}

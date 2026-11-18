package claude

import (
	"encoding/json"
	"os"
)

func writeTestJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

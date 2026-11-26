package pluginfeed

import (
	"path/filepath"
	"strings"

	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/statefile"
)

func (s pluginFiles) savePluginIdentity(id string) error {
	if !model.IsCanonicalUUID(id) || s.checkName() != nil {
		return statefile.ErrInvalidFile
	}
	raw, err := statefile.EncodeJSON(s.name, 128)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(filepath.Join(s.pluginDir(), "id-"+id+".json"), raw)
}

// A bounded hint locates a name; only the current installation is authority.
// Caller holds Store flock. Native registration and name resolution stay intact.
func pluginInboxTarget(root, to string) (pluginFiles, pluginRecord, error) {
	var p pluginRecord
	f := pluginFiles{root: root}
	if !model.IsPluginInboxAddress(to) || pluginDirectory(f.pluginDir(), false) != nil {
		return f, p, model.NewError(model.CodeUnauthorized, "plugin inbox unavailable")
	}
	raw, err := statefile.ReadBytes(filepath.Join(f.pluginDir(), "id-"+strings.TrimPrefix(to, "plugin:")+".json"), 128)
	if err == nil {
		err = statefile.DecodeStrictRecordJSON(raw, &f.name)
	}
	if err == nil {
		p, _, err = f.loadPlugin()
	}
	if err != nil || !p.Enabled || to != "plugin:"+p.ID {
		return f, p, model.NewError(model.CodeUnauthorized, "plugin inbox unavailable")
	}
	return f, p, nil
}

func syncPluginIdentity(s pluginFiles, id string, enabled bool) error {
	if !enabled {
		return nil
	}
	raw, err := statefile.ReadBytes(filepath.Join(s.pluginDir(), "id-"+id+".json"), 128)
	var name string
	if err == nil && statefile.DecodeStrictRecordJSON(raw, &name) == nil && name == s.name {
		return nil
	}
	return s.savePluginIdentity(id)
}

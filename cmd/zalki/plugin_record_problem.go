package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/statefile"
)

const invalidPluginNameDetail = "invalid plugin name; expected [a-z][a-z0-9-]{0,31}"

// pluginRecordCause names why a plugin record or folder cannot be read
// safely. It is always one fixed class and never error text: a strict
// decode error can quote the record's own field names or values.
func pluginRecordCause(err error) string {
	var pathErr *fs.PathError
	switch {
	case errors.Is(err, statefile.ErrInvalidFile):
		return "it is not a safe regular file"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	case errors.As(err, &pathErr):
		return "it could not be read"
	}
	return "its content is not a valid plugin record"
}

func pluginRecordPath(store bus.Store, name string) string {
	return filepath.Join(store.Dir(), "plugins", name+".json")
}

// pluginRecordDetail is the cause and the file, as a list line shows them.
func pluginRecordDetail(store bus.Store, name, cause string) string {
	if len(name) > 32 || !model.ValidKind(name) {
		return invalidPluginNameDetail
	}
	return cause + "; the record is " + shellQuote(pluginRecordPath(store, name))
}

// pluginRecordProblem is the error for one record that cannot be read.
func pluginRecordProblem(store bus.Store, name string, err error) error {
	// Show accepts an arbitrary argument; backend name rejection happens before
	// any record read. Do not invent a record path or echo control bytes for it.
	if len(name) > 32 || !model.ValidKind(name) {
		return errors.New("cannot safely read plugin: " + invalidPluginNameDetail)
	}
	return fmt.Errorf("cannot safely read plugin %s: %s", strconv.QuoteToASCII(name), pluginRecordDetail(store, name, pluginRecordCause(err)))
}

// pluginsFolderProblem is the error for a plugins folder that cannot be
// read: a link or a file where the folder should be, or a read error.
func pluginsFolderProblem(dir string, err error) error {
	if err == nil {
		return fmt.Errorf("the plugins folder %s is not a safe folder (it is a link or a file)", shellQuote(dir))
	}
	cause := "it could not be read"
	if errors.Is(err, os.ErrPermission) {
		cause = "permission denied"
	}
	return fmt.Errorf("cannot read the plugins folder %s: %s", shellQuote(dir), cause)
}

// sessionListProblem is the error when review targets cannot be listed: a
// fixed text, never the store's error text.
func sessionListProblem(err error, skipped int) error {
	if err != nil {
		return errors.New("could not safely list running sessions: the session records could not be read")
	}
	return fmt.Errorf("could not safely list running sessions: %d session records could not be read; zalki doctor names them and what to do", skipped)
}

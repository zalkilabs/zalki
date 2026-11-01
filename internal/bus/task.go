package bus

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// taskLimit is the maximum stored claim, in bytes.
const taskLimit = 120

// TaskResult is the set_task tool result. An empty task means the claim was cleared.
type TaskResult struct {
	Task string `json:"task,omitempty"`
}

func validTask(task string) error {
	if task == "" {
		return nil
	}
	if strings.TrimSpace(task) != task || len(task) > taskLimit || !utf8.ValidString(task) || strings.ContainsFunc(task, unicode.IsControl) {
		return fail(CodeInvalidInput, "task must be one line, valid UTF-8, at most 120 bytes, with no surrounding space")
	}
	return nil
}

// SetTask stores the caller's own claim. An empty task clears it.
// The claim is not an address and does not change routing.
func (s *Service) SetTask(actor, key, task string) (TaskResult, error) {
	if err := validTask(task); err != nil {
		return TaskResult{}, err
	}
	var result TaskResult
	err := s.Store.locked(func() error {
		current, err := s.Store.authenticate(actor, key)
		if err != nil {
			return err
		}
		current.Task = task
		if err := s.Store.saveSession(current); err != nil {
			return err
		}
		result.Task = task
		return nil
	})
	return result, err
}

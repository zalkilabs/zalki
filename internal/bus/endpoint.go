package bus

import (
	"errors"

	"github.com/zalkilabs/zalki/internal/model"
)

// InspectEndpoint returns the recorded form of the socket identity at path.
func InspectEndpoint(path string) (*model.Endpoint, error) {
	id, err := InspectSocket(path)
	if err != nil {
		return nil, err
	}
	endpoint := model.Endpoint(id)
	return &endpoint, nil
}

func CheckEndpoint(expected *model.Endpoint) error {
	if expected == nil {
		return errors.New("no bound messaging socket")
	}
	current, err := InspectEndpoint(expected.Path)
	if err != nil {
		return err
	}
	if *current != *expected {
		return errors.New("messaging socket was replaced; its listener must register again")
	}
	return nil
}

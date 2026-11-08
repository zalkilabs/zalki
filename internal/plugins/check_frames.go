package plugins

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/zalkilabs/zalki/internal/model"
)

//go:embed testdata/conformance/frames.jsonl
var checkFrameVectors string

func checkOffers() ([]frame, error) {
	decoder := json.NewDecoder(strings.NewReader(checkFrameVectors))
	var event, gap frame
	for {
		var row struct {
			ID    string `json:"id"`
			Bytes string `json:"bytes_utf8"`
		}
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if row.ID != "event" && row.ID != "enabled_gap" {
			continue
		}
		f, err := readFrame(bufio.NewReaderSize(strings.NewReader(row.Bytes), maxFrame+1))
		if err != nil {
			return nil, err
		}
		if row.ID == "event" {
			event = f
		} else {
			gap = f
		}
	}
	if event.Event == nil || gap.Gap != "enabled" {
		return nil, errors.New("missing conformance offer fixtures")
	}
	if err := event.Event.Validate(); err != nil {
		return nil, err
	}
	second := *event.Event
	second.ID = "00000000-0000-4000-8000-000000000006"
	second.TransactionID = "00000000-0000-4000-8000-000000000007"
	second.Session.Previous, second.Session.State = model.StateBusy, model.StateReady
	return []frame{gap, event, event, {ID: second.ID, Event: &second}}, nil
}

package pluginfeed

import (
	"bytes"
	"testing"

	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestStreamPrivateCheckpointPhysicalByteBound(t *testing.T) {
	feed, _, _, _, _ := streamPendingMessageFixture(t)
	image := checkpointImage(t, feed)
	for _, size := range []int{524288, 524289} {
		padded := paddedStreamJSON(image, size)
		if err := fsutil.WriteAtomic(feed.streamPath(), padded); err != nil {
			t.Fatal(err)
		}
		budget := feed.stream.budget(t.Context())
		_, exists, err := feed.loadStreamState(budget)
		if size == 524288 {
			if err != nil || !exists || budget.used[1] != 1 {
				t.Fatal("F_CHECKPOINT_EXACT_PHYSICAL_LIMIT_POSITIVE", err)
			}
		} else if err == nil || budget.used[0] != 0 || budget.used[1] != 0 || budget.used[2] != 0 || budget.used[4] != 0 {
			t.Fatal("F_CHECKPOINT_OVER_PHYSICAL_LIMIT_BEFORE_READ_OR_PROBE", err)
		}
	}
}

func paddedStreamJSON(raw []byte, size int) []byte {
	base := bytes.TrimSuffix(raw, []byte{'\n'})
	result := append(bytes.Clone(base), bytes.Repeat([]byte{' '}, size-len(base)-1)...)
	return append(result, '\n')
}

func TestStreamSingleFramePhysicalByteBoundIncludesLF(t *testing.T) {
	raw, err := encodeStreamFrame(streamFrame{ID: model.NewUUID(), Gap: "resource"})
	if err != nil {
		t.Fatal(err)
	}
	exact := append(bytes.Clone(raw[:len(raw)-1]), bytes.Repeat([]byte{' '}, 65535-len(raw))...)
	exact = append(exact, '}')
	if len(exact) != 65535 || validateStreamFrame(exact) != nil {
		t.Fatal("F_PLAIN_FRAME_EXACT_BYTES_PLUS_LF_POSITIVE")
	}
	over := append(bytes.Clone(exact[:len(exact)-1]), ' ', '}')
	if validateStreamFrame(over) == nil {
		t.Fatal("F_PLAIN_FRAME_FINAL_LF_COUNTS_AGAINST_LIMIT")
	}
}

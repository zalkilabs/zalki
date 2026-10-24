package bus

import (
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkMessageLookup(b *testing.B) {
	for _, size := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			store := Store{Dir: filepath.Join(b.TempDir(), "state")}
			receiver, key, err := store.Register("cursor", newID(), "/fixture", nil)
			if err != nil {
				b.Fatal(err)
			}
			var first string
			for i := range size {
				msg := Message{ID: newID(), From: "codex:" + newID(), To: receiver.ID, Body: "review result",
					CreatedAt: timestamp(), TargetGeneration: receiver.Generation, Status: StatusSubmitted}
				if i == 0 {
					first = msg.ID
				}
				if err := store.saveMessage(msg); err != nil {
					b.Fatal(err)
				}
			}
			for _, exact := range []bool{false, true} {
				name := "inbox"
				query := MessageQuery{Actor: receiver.ID, Capability: key}
				if exact {
					name = "exact"
					query.MessageID = first
				}
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if _, err := store.ReadMessages(query); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

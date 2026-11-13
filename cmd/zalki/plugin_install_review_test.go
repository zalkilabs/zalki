package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestPluginReviewScreens(t *testing.T) {
	raw, err := os.ReadFile("testdata/plugin-review-screens.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string][]string
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	for name, want := range expected {
		t.Run(name, func(t *testing.T) {
			p := pluginReviewPlan{name: name, id: "zalkilabs/" + name, version: "0.1.0", source: "/owner/dev/code/zalki-plugins/" + name, home: "/owner", build: []string{"go", "build", "-trimpath", "-o", "plugin", "."}, run: []string{"plugin"}, access: pluginReviewAccess{status: true}}
			switch name {
			case "dashboard":
				p.build = []string{"go", "build", "-mod=readonly", "-trimpath", "-o", "plugin", "."}
			case "arbiter":
				p.version = "0.3.1"
				p.access.receive = true
			case "spawner":
				p.access.receive, p.access.send = true, true
				p.build = []string{"go", "build", "-p=1", "-mod=readonly", "-trimpath", "-o", "plugin", "./cmd/spawner"}
				p.run = []string{"plugin", "serve"}
				p.candidates = []pluginReviewTarget{{reviewActorA, "zlaki"}, {reviewActorB, "worker-3"}}
				p.send = []string{reviewActorA, reviewActorB}
			}
			got, err := renderPluginReview(p)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("screen differs from accepted copy: %v\ngot %#v\nwant %#v", err, got, want)
			}
		})
	}
}

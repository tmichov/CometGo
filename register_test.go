package cometgo

import "testing"

// Registering after Start would race the engine's unlocked map reads.
func TestRegisterAfterStartPanics(t *testing.T) {
	e := setup(t) // started
	const want = "cometgo: register publications and methods before Start"
	for name, register := range map[string]func(){
		"Publish": func() {
			Publish(e.app, "late", PubOptions{Tables: []string{e.table}}, func(*SubCtx, struct{}) (Query, error) { return Query{}, nil })
		},
		"Method": func() {
			Method(e.app, "late", func(*Call, struct{}) (struct{}, error) { return struct{}{}, nil })
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != want {
					t.Fatalf("panic = %v, want %q", r, want)
				}
			}()
			register()
		})
	}
}

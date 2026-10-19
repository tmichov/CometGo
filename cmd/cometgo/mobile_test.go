package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMobileInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Todo")
	var out, errb bytes.Buffer
	if code := run([]string{"mobile", "init", "--port", "9090", dir}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, f := range []string{"package.json", "app.config.js", "index.ts", "App.tsx", "comet.ts", "comet-url.ts", "schema.ts", "tsconfig.json", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	var pkg struct {
		Name string
		Deps map[string]string `json:"dependencies"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatalf("package.json: %v", err)
	}
	if pkg.Name != "my-todo" || pkg.Deps["@cometgo/client"] != clientDep {
		t.Fatalf("package.json %+v", pkg)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "app.config.js"))
	for _, want := range []string{`name: "My Todo"`, `bundleIdentifier: "com.example.my_todo"`, `package: "com.example.my_todo"`, "COMET_INSECURE"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("app.config.js lacks %s", want)
		}
	}
	if c, _ := os.ReadFile(filepath.Join(dir, "comet.ts")); !strings.Contains(string(c), `"9090"`) {
		t.Error("comet.ts lacks the port")
	}
	for _, f := range []string{"package.json.tmpl", "gitignore"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s left in the app", f)
		}
	}
}

func TestMobileInitRefusesANonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x"), nil, 0o644)
	var out, errb bytes.Buffer
	if code := run([]string{"mobile", "init", dir}, &out, &errb); code == 0 || !strings.Contains(errb.String(), "not empty") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

func TestMobileInitValidates(t *testing.T) {
	for _, args := range [][]string{
		{"--bundle-id", "todo"},
		{"--bundle-id", "com.1example.todo"},
		{"--name", "!!!"},
		{"--port", "0"},
	} {
		var out, errb bytes.Buffer
		if code := run(append([]string{"mobile", "init"}, append(args, filepath.Join(t.TempDir(), "app"))...), &out, &errb); code == 0 {
			t.Errorf("%v accepted", args)
		}
	}
	// A name starting with a digit still gets a valid default bundle id.
	var out, errb bytes.Buffer
	dir := filepath.Join(t.TempDir(), "2do")
	if code := run([]string{"mobile", "init", dir}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

func TestUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "mobile init") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

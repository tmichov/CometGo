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
	for _, want := range []string{`name: "My Todo"`, `bundleIdentifier: "com.example.mytodo"`, `package: "com.example.mytodo"`, "COMET_INSECURE"} {
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
	// Names that would not make a valid id on their own get a prefix.
	for name, want := range map[string]string{"2do": "com.example.app2do", "new": "com.example.appnew", "my-app": "com.example.myapp"} {
		var out, errb bytes.Buffer
		dir := filepath.Join(t.TempDir(), name)
		if code := run([]string{"mobile", "init", dir}, &out, &errb); code != 0 {
			t.Fatalf("%s: exit %d: %s", name, code, errb.String())
		}
		if cfg, _ := os.ReadFile(filepath.Join(dir, "app.config.js")); !strings.Contains(string(cfg), `package: "`+want+`"`) {
			t.Errorf("%s: want %s in %s", name, want, cfg)
		}
	}
}

func TestBundleIDRules(t *testing.T) {
	for id, ok := range map[string]bool{
		"com.example.todo": true, "com.Example.Todo2": true,
		"com.example.my_todo": false, // iOS refuses _
		"com.example.my-todo": false, // Android refuses -
		"com.example.new":     false, // a Java keyword
		"todo":                false, "com.1example.todo": false, "com..todo": false,
	} {
		if got := validBundleID(id) == ""; got != ok {
			t.Errorf("%s: valid %v, want %v", id, got, ok)
		}
	}
}

func TestMobileInitClientAndQuoting(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	os.WriteFile("client.tgz", []byte("x"), 0o644)
	var out, errb bytes.Buffer
	dir := filepath.Join(work, "app")
	if code := run([]string{"mobile", "init", "--name", `Say "hi" \ yes`, "--client", "client.tgz", dir, "--port", "9001"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var pkg struct {
		Deps map[string]string `json:"dependencies"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatalf("package.json is not JSON: %v", err)
	}
	if want := "file:" + filepath.Join(work, "client.tgz"); pkg.Deps["@cometgo/client"] != want {
		t.Fatalf("client dep %q, want %q", pkg.Deps["@cometgo/client"], want)
	}
	if cfg, _ := os.ReadFile(filepath.Join(dir, "app.config.js")); !strings.Contains(string(cfg), `name: "Say \"hi\" \\ yes"`) {
		t.Errorf("name not quoted as JSON: %s", cfg)
	}
	if c, _ := os.ReadFile(filepath.Join(dir, "comet.ts")); !strings.Contains(string(c), `"9001"`) {
		t.Error("a flag after the directory was ignored")
	}
	if entries, _ := os.ReadDir(work); len(entries) != 2 {
		t.Errorf("leftovers next to the app: %v", entries)
	}
}

func TestUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "mobile init") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

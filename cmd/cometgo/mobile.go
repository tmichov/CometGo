package main

import (
	"bytes"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

//go:embed template/mobile
var mobileTemplate embed.FS

// clientDep is the @cometgo/client version the template depends on: the
// first with @cometgo/client/react-native.
const clientDep = "^0.2.0"

type mobileApp struct {
	Name, Slug, BundleID, Port, ClientDep string
}

var (
	// Android package and iOS bundle id rules together: two or more dot
	// separated segments, each a letter followed by letters, digits or _.
	bundleIDRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	slugRe     = regexp.MustCompile(`[^a-z0-9]+`)
)

func mobileInit(args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("cometgo mobile init", flag.ContinueOnError)
	fl.SetOutput(stderr)
	name := fl.String("name", "", "app name shown on the device (default: the directory name)")
	bundleID := fl.String("bundle-id", "", "Android package and iOS bundle id, e.g. com.example.todo (default: com.example.<name>)")
	port := fl.Int("port", 8080, "the CometGo server's port in development")
	client := fl.String("client", clientDep, "the @cometgo/client dependency: a version, or a path or tarball for a local build")
	fl.Usage = func() {
		fmt.Fprintln(stderr, "Usage: cometgo mobile init [flags] [dir]\n\nCreates an Expo (React Native) app in dir (default: mobile) that connects to your CometGo server.\n\nFlags:")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() > 1 {
		return errors.New("at most one directory")
	}
	dir := "mobile"
	if fl.NArg() == 1 {
		dir = fl.Arg(0)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	app := mobileApp{Name: *name, BundleID: *bundleID, Port: strconv.Itoa(*port), ClientDep: *client}
	if app.Name == "" {
		app.Name = filepath.Base(abs)
	}
	app.Slug = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(app.Name), "-"), "-")
	if app.Slug == "" {
		return fmt.Errorf("name %q has no letters or digits", app.Name)
	}
	if app.BundleID == "" {
		app.BundleID = "com.example." + strings.ReplaceAll(app.Slug, "-", "_")
		if app.Slug[0] >= '0' && app.Slug[0] <= '9' {
			app.BundleID = "com.example.app_" + strings.ReplaceAll(app.Slug, "-", "_")
		}
	}
	if !bundleIDRe.MatchString(app.BundleID) {
		return fmt.Errorf("bundle id %q: use two or more dot-separated parts, each starting with a letter (com.example.todo)", app.BundleID)
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("port %d out of range", *port)
	}
	if err := writeMobileApp(abs, app); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Created %s in %s.\n\nNext:\n  cd %s\n  npm install\n  cometgo run android   # or ios, on macOS\n\nThen generate the server's schema into %s (docs/codegen.md).\n",
		app.Name, dir, dir, filepath.Join(dir, "schema.ts"))
	if _, err := exec.LookPath("npm"); err != nil {
		fmt.Fprintln(stdout, "\nnpm was not found: install Node.js 20 or later (https://nodejs.org) first.")
	}
	return nil
}

// writeMobileApp copies the template into dir, which must be empty or
// missing. *.tmpl files are rendered with app; "gitignore" becomes
// ".gitignore" (a dotfile in the template would be left out of the embed).
func writeMobileApp(dir string, app mobileApp) error {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	root := "template/mobile"
	return fs.WalkDir(mobileTemplate, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := mobileTemplate.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(out, ".tmpl") {
			t, err := template.New(rel).Option("missingkey=error").Parse(string(b))
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			if err := t.Execute(&buf, app); err != nil {
				return err
			}
			b, out = buf.Bytes(), strings.TrimSuffix(out, ".tmpl")
		}
		if filepath.Base(out) == "gitignore" {
			out = filepath.Join(filepath.Dir(out), ".gitignore")
		}
		return os.WriteFile(out, b, 0o644)
	})
}

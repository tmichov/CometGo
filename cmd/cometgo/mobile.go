package main

import (
	"bytes"
	"embed"
	"encoding/json"
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
	// What both platforms accept: Android segments start with a letter and
	// allow _, iOS allows - but not _, so neither. Two or more segments.
	bundleIDRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(\.[A-Za-z][A-Za-z0-9]*)+$`)
	slugRe     = regexp.MustCompile(`[^a-z0-9]+`)
	// Java keywords and literals, which Android refuses as package segments
	// (the list Expo's own check uses).
	javaReserved = map[string]bool{}
)

func init() {
	for _, w := range strings.Fields("abstract assert boolean break byte case catch char class const continue default do double else enum extends final finally float for goto if implements import instanceof int interface long native new package private protected public return short static super switch synchronized this throw throws transient try void volatile while true false null strictfp") {
		javaReserved[w] = true
	}
}

// validBundleID reports why id cannot be an Android package and iOS bundle
// id, or "" if it can.
func validBundleID(id string) string {
	if !bundleIDRe.MatchString(id) {
		return "use two or more dot-separated parts of letters and digits, each starting with a letter (com.example.todo)"
	}
	for _, seg := range strings.Split(id, ".") {
		if javaReserved[seg] {
			return fmt.Sprintf("%q is a Java keyword, which Android refuses in a package name", seg)
		}
	}
	return ""
}

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
	dir := "mobile"
	if fl.NArg() > 0 {
		// Flags may also follow the directory.
		dir = fl.Arg(0)
		if err := fl.Parse(fl.Args()[1:]); err != nil {
			return err
		}
		if fl.NArg() > 0 {
			return errors.New("at most one directory")
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	// A path on disk becomes an absolute file: dependency: npm would read a
	// relative one from the app's directory, not from here.
	clientSpec := *client
	if _, err := os.Stat(clientSpec); err == nil {
		p, err := filepath.Abs(clientSpec)
		if err != nil {
			return err
		}
		clientSpec = "file:" + p
	}
	app := mobileApp{Name: *name, BundleID: *bundleID, Port: strconv.Itoa(*port), ClientDep: clientSpec}
	if app.Name == "" {
		app.Name = filepath.Base(abs)
	}
	app.Slug = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(app.Name), "-"), "-")
	if app.Slug == "" {
		return fmt.Errorf("name %q has no ASCII letters or digits to build the app's slug from; pass --name (and --bundle-id)", app.Name)
	}
	if app.BundleID == "" {
		last := strings.ReplaceAll(app.Slug, "-", "")
		if last[0] >= '0' && last[0] <= '9' || javaReserved[last] {
			last = "app" + last
		}
		app.BundleID = "com.example." + last
	}
	if why := validBundleID(app.BundleID); why != "" {
		return fmt.Errorf("bundle id %q: %s", app.BundleID, why)
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("port %d out of range", *port)
	}
	if err := writeMobileApp(abs, app); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Created %s in %s.\n\nNext:\n  cd %q\n  npm install\n  npx expo run:android   # or run:ios, on macOS\n\nIn development the app connects to your server on port %s of the machine Metro runs on; the server must listen on all interfaces (\":%s\"). Generate the server's schema into %s (docs/codegen.md).\n",
		app.Name, dir, dir, app.Port, app.Port, filepath.Join(dir, "schema.ts"))
	if _, err := exec.LookPath("npm"); err != nil {
		fmt.Fprintln(stdout, "\nnpm was not found: install Node.js 20 or later (https://nodejs.org) first.")
	}
	return nil
}

// writeMobileApp copies the template into dir, which must be empty or
// missing. *.tmpl files are rendered with app (the json function quotes a
// value for JSON and JavaScript alike); "gitignore" becomes ".gitignore" (a
// dotfile in the template would be left out of the embed). It writes into a
// temporary sibling and renames it, so a failure leaves no half app.
func writeMobileApp(dir string, app mobileApp) error {
	exists := false
	if entries, err := os.ReadDir(dir); err == nil {
		if len(entries) > 0 {
			return fmt.Errorf("%s is not empty", dir)
		}
		exists = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := renderTemplate(tmp, app); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	if exists {
		if err := os.Remove(dir); err != nil { // empty, checked above
			return err
		}
	}
	return os.Rename(tmp, dir)
}

var templateFuncs = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
}

func renderTemplate(dir string, app mobileApp) error {
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
			t, err := template.New(rel).Option("missingkey=error").Funcs(templateFuncs).Parse(string(b))
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

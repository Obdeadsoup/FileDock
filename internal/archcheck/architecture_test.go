package archcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type layer string

const (
	command   layer = "command"
	config    layer = "config"
	httpapi   layer = "HTTP API"
	business  layer = "business"
	store     layer = "storage adapter"
	archcheck layer = "architecture test"
)

var packageLayers = map[string]layer{
	"cmd/filedockd":            command,
	"internal/config":          config,
	"internal/httpapi":         httpapi,
	"internal/archcheck":       archcheck,
	"internal/files":           business,
	"internal/accounts":        business,
	"internal/store/sqlite":    store,
	"internal/store/localdisk": store,
}

var businessStdlib = []string{
	"bytes", "context", "errors", "fmt", "io", "path", "slices", "strconv", "strings", "sync", "time",
}

var accountsCryptoStdlib = []string{
	"crypto/rand", "crypto/sha256", "crypto/subtle",
}

func packageLayer(modulePath, importPath string) layer {
	relative, local := strings.CutPrefix(importPath, modulePath+"/")
	if !local {
		return ""
	}
	return packageLayers[relative]
}

func dependencyViolation(modulePath, importer, imported string, standard bool) string {
	from := packageLayer(modulePath, importer)
	if from == "" {
		return "importing package has no layer assignment"
	}
	if from == archcheck {
		return "architecture checks belong in test files"
	}
	if standard {
		switch from {
		case command, config, store:
			return ""
		case httpapi:
			if imported == "net/http" || imported == "encoding/json" || imported == "mime" {
				return ""
			}
		}
		if importer == modulePath+"/internal/accounts" && slices.Contains(accountsCryptoStdlib, imported) {
			return ""
		}
		if slices.Contains(businessStdlib, imported) {
			return ""
		}
		return string(from) + " may only import its approved standard library packages"
	}

	to := packageLayer(modulePath, imported)
	switch to {
	case "":
		if imported == modulePath || strings.HasPrefix(imported, modulePath+"/") {
			return "imported package has no layer assignment"
		}
		if importer == modulePath+"/internal/store/sqlite" && imported == "modernc.org/sqlite" {
			return ""
		}
		if importer == modulePath+"/internal/accounts" && imported == "golang.org/x/crypto/argon2" {
			return ""
		}
		return "external dependencies are forbidden"
	case command:
		return "production packages may not import the command entry point"
	case archcheck:
		return "production packages may not import architecture tests"
	}
	switch from {
	case command:
		return ""
	case httpapi, store:
		if to == business {
			return ""
		}
	}
	return string(from) + " may not import " + string(to)
}

type listedPackage struct {
	ImportPath string
	Imports    []string
	Standard   bool
	Module     *struct {
		Path string
		Main bool
	}
}

func TestProductionDependencies(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// Go's test cache cannot see files read by subprocesses. Walking the Go
	// package directories makes it track their entries, including new packages.
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() || path == root {
			return err
		}
		name := entry.Name()
		if name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module directories: %v", err)
	}

	cmd := exec.Command("go", "list", "-deps", "-json=ImportPath,Imports,Standard,Module", "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}

	decoder := json.NewDecoder(bytes.NewReader(output))
	var modulePackages []listedPackage
	standardPackages := make(map[string]bool)
	for {
		var pkg listedPackage
		err := decoder.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		if pkg.Standard {
			standardPackages[pkg.ImportPath] = true
		}
		if pkg.Module != nil && pkg.Module.Main {
			modulePackages = append(modulePackages, pkg)
		}
	}
	if len(modulePackages) == 0 {
		t.Fatal("go list returned no packages in the main module")
	}
	for _, pkg := range modulePackages {
		if len(pkg.Imports) == 0 && packageLayer(pkg.Module.Path, pkg.ImportPath) == "" {
			t.Errorf("%s: package has no layer assignment", pkg.ImportPath)
		}
		for _, imported := range pkg.Imports {
			if rule := dependencyViolation(pkg.Module.Path, pkg.ImportPath, imported, standardPackages[imported]); rule != "" {
				t.Errorf("%s -> %s: %s", pkg.ImportPath, imported, rule)
			}
		}
	}
}

func TestDependencyRules(t *testing.T) {
	const module = "example.com/archcheck-fixture"
	const (
		entry    = module + "/cmd/filedockd"
		cfg      = module + "/internal/config"
		api      = module + "/internal/httpapi"
		files    = module + "/internal/files"
		accounts = module + "/internal/accounts"
		sqlite   = module + "/internal/store/sqlite"
		disk     = module + "/internal/store/localdisk"
		unknown  = module + "/internal/unknown"
	)
	for _, tc := range []struct {
		importer, imported string
		standard           bool
		violation          bool
	}{
		{entry, cfg, false, false},
		{entry, disk, false, false},
		{entry, accounts, false, false},
		{api, "net/http", true, false},
		{api, "mime", true, false},
		{api, files, false, false},
		{api, accounts, false, false},
		{files, "io", true, false},
		{accounts, "crypto/rand", true, false},
		{accounts, "crypto/sha256", true, false},
		{accounts, "crypto/subtle", true, false},
		{accounts, "golang.org/x/crypto/argon2", false, false},
		{sqlite, files, false, false},
		{sqlite, accounts, false, false},
		{sqlite, "database/sql", true, false},
		{sqlite, "modernc.org/sqlite", false, false},
		{sqlite, "github.com/other/pkg", false, true},
		{sqlite, "golang.org/x/crypto/argon2", false, true},
		{disk, "os", true, false},
		{disk, "modernc.org/sqlite", false, true},
		{api, "modernc.org/sqlite", false, true},
		{api, "golang.org/x/crypto/argon2", false, true},
		{api, "crypto/rand", true, true},
		{files, "modernc.org/sqlite", false, true},
		{files, "golang.org/x/crypto/argon2", false, true},
		{files, "crypto/rand", true, true},
		{files, "crypto/sha256", true, true},
		{files, "mime", true, true},
		{accounts, "mime", true, true},
		{accounts, "modernc.org/sqlite", false, true},
		{accounts, files, false, true},
		{files, accounts, false, true},
		{entry, "modernc.org/sqlite", false, true},
		{cfg, "modernc.org/sqlite", false, true},
		{api, sqlite, false, true},
		{api, "database/sql", true, true},
		{api, "os", true, true},
		{files, "net/http", true, true},
		{files, "database/sql", true, true},
		{files, "os", true, true},
		{files, cfg, false, true},
		{files, disk, false, true},
		{cfg, entry, false, true},
		{entry, unknown, false, true},
		{unknown, "fmt", true, true},
	} {
		if rule := dependencyViolation(module, tc.importer, tc.imported, tc.standard); (rule != "") != tc.violation {
			t.Errorf("%s -> %s: rule = %q, want violation = %t", tc.importer, tc.imported, rule, tc.violation)
		}
	}
}

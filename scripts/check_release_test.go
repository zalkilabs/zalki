package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const fixtureVersion = "0.1.0-rc.1"

func archive(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := []byte(files[name])
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	for _, close := range []func() error{tw.Close, gz.Close, f.Close} {
		if err := close(); err != nil {
			t.Fatal(err)
		}
	}
}

func sourceFiles() map[string]string {
	files := map[string]string{}
	for _, name := range []string{"go.mod", "go.sum", "cmd/zalki/main.go", "LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.txt"} {
		files["zalki-source/"+name] = "fixture\n"
	}
	return files
}

func binaryFiles() map[string]string {
	files := map[string]string{}
	for _, name := range []string{"LICENSE", "NOTICE", "THIRD_PARTY_NOTICES.txt", "README.md", "completions/zalki.bash", "completions/_zalki", "completions/zalki.fish"} {
		files[name] = "fixture\n"
	}
	files["zalki"] = "#!/bin/sh\ncase \"$1\" in\nversion) printf '" + fixtureVersion + "+fixture\\n';;\n--help) printf 'Local messaging\\n';;\n*) exit 1;;\nesac\n"
	return files
}

func checksums(t *testing.T, dir string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&data, "%x  %s\n", sha256.Sum256(content), filepath.Base(path))
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) string {
	t.Helper()
	for _, tool := range []string{"sh", "tar", "shasum", "ruby"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("release fixture requires %s: %v", tool, err)
		}
	}
	dir := t.TempDir()
	archive(t, filepath.Join(dir, "zalki_"+fixtureVersion+"_source.tar.gz"), sourceFiles())
	for _, arch := range []string{"amd64", "arm64"} {
		files := binaryFiles()
		files["zalki"] += "# " + arch + " fixture\n"
		archive(t, filepath.Join(dir, "zalki_"+fixtureVersion+"_darwin_"+arch+".tar.gz"), files)
	}
	checksums(t, dir)
	return dir
}

func runCheck(t *testing.T, dir, expected string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "check-release.sh", dir, expected)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestReleaseArchivesAndCask(t *testing.T) {
	dir := fixture(t)
	if output, err := runCheck(t, dir, fixtureVersion); err != nil {
		t.Fatal(output, err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	cask, err := os.ReadFile(filepath.Join(dir, "zalki.rb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, platform := range []struct{ arch, key string }{{"arm64", "arm"}, {"amd64", "intel"}} {
		name := "zalki_" + fixtureVersion + "_darwin_" + platform.arch + ".tar.gz"
		binary, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		checksum := regexp.QuoteMeta(platform.key) + `:\s+"` + fmt.Sprintf("%x", sha256.Sum256(binary)) + `"`
		if !regexp.MustCompile(checksum).Match(cask) {
			t.Fatalf("cask does not pin the correct %s archive checksum", platform.arch)
		}
	}
	for _, required := range []string{`cask "zalki"`, `arch arm: "arm64", intel: "amd64"`, `version "` + fixtureVersion + `"`,
		`url "https://github.com/zalkilabs/zalki/releases/download/v#{version}/zalki_#{version}_darwin_#{arch}.tar.gz"`,
		`binary "zalki"`, `bash_completion "completions/zalki.bash"`, `zsh_completion "completions/_zalki"`, `fish_completion "completions/zalki.fish"`} {
		if !bytes.Contains(cask, []byte(required)) {
			t.Fatalf("missing cask artifact or architecture metadata: %s", required)
		}
	}
	if bytes.Contains(cask, []byte("_source.tar.gz")) {
		t.Fatal("published cask still downloads source")
	}
	if !bytes.Contains(manifest, []byte(fmt.Sprintf("%x  zalki.rb", sha256.Sum256(cask)))) {
		t.Fatal("cask missing from release checksums")
	}
	if output, err := runCheck(t, dir, fixtureVersion); err != nil {
		t.Fatal(output, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if !bytes.Equal(manifest, after) {
		t.Fatal("repeated checks changed the release manifest")
	}
}

func TestInvalidCaskInputs(t *testing.T) {
	dir := fixture(t)
	arm64 := filepath.Join(dir, "zalki_"+fixtureVersion+"_darwin_arm64.tar.gz")
	amd64 := filepath.Join(dir, "zalki_"+fixtureVersion+"_darwin_amd64.tar.gz")
	for name, args := range map[string][]string{
		"missing-architecture":  {fixtureVersion, arm64},
		"wrong-version":         {"9.9.9", arm64, amd64},
		"swapped-architectures": {fixtureVersion, amd64, arm64},
		"source-archive":        {fixtureVersion, filepath.Join(dir, "zalki_"+fixtureVersion+"_source.tar.gz"), amd64},
		"invalid-version":       {"../release", arm64, amd64},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("sh", append([]string{"homebrew-cask.sh"}, args...)...)
			if out, err := cmd.Output(); err == nil || len(out) != 0 {
				t.Fatalf("invalid input produced a cask: err=%v, output=%s", err, out)
			}
		})
	}
}

func TestUnsignedReleaseCannotPassSigningGate(t *testing.T) {
	dir := fixture(t)
	cmd := exec.Command("sh", "check-release.sh", dir, fixtureVersion)
	cmd.Env = append(os.Environ(), "ZALKI_REQUIRE_SIGNED=1")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatal("unsigned fixture accepted as a signed release", string(out))
	}
	if _, err := os.Stat(filepath.Join(dir, "zalki.rb")); !os.IsNotExist(err) {
		t.Fatal("unsigned release produced a publishable cask", err)
	}
}

func TestInvalidReleaseCannotPass(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"corrupt-archive", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "zalki_"+fixtureVersion+"_source.tar.gz"), []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing-source-file", func(t *testing.T, dir string) {
			files := sourceFiles()
			delete(files, "zalki-source/go.mod")
			files["zalki-source/goXmod"] = "not a module file"
			archive(t, filepath.Join(dir, "zalki_"+fixtureVersion+"_source.tar.gz"), files)
			checksums(t, dir)
		}},
		{"missing-completion", func(t *testing.T, dir string) {
			files := binaryFiles()
			delete(files, "completions/_zalki")
			archive(t, filepath.Join(dir, "zalki_"+fixtureVersion+"_darwin_amd64.tar.gz"), files)
			checksums(t, dir)
		}},
		{"missing-checksum-entry", func(t *testing.T, dir string) {
			path := filepath.Join(dir, "checksums.txt")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.SplitAfter(string(data), "\n")
			if err := os.WriteFile(path, []byte(strings.Join(lines[1:], "")), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := fixture(t)
			tc.mutate(t, dir)
			if output, err := runCheck(t, dir, fixtureVersion); err == nil {
				t.Fatal("invalid release accepted", output)
			}
			if _, err := os.Stat(filepath.Join(dir, "zalki.rb")); !os.IsNotExist(err) {
				t.Fatal("invalid release produced a cask", err)
			}
		})
	}
	t.Run("wrong-tag", func(t *testing.T) {
		if output, err := runCheck(t, fixture(t), "9.9.9"); err == nil {
			t.Fatal("wrong release tag accepted", output)
		}
	})
}

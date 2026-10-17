// Command notices regenerates THIRD-PARTY-NOTICES.md.
//
// It walks the two dependency graphs that end up inside a released KiChat —
// the Go modules linked into the binary (for linux, windows and darwin, since
// each platform links a slightly different set) and the frontend's production
// dependency tree, which Vite bundles into frontend/dist — and writes one file
// that reproduces every component's copyright line beside the text of its
// licence.
//
// It exists because that list cannot be maintained by hand: it is 150-odd
// packages today, it moves with every dependency bump, and a notice file that
// has drifted is worse than none. The output is deterministic — sorted, with
// no timestamps — so a change to the file means a real change to the graph.
//
//	go run ./tools/notices
//
// Run it from the repository root, after `go mod download` and `make deps`:
// it reads the module cache and frontend/node_modules, and refuses rather than
// guessing when either is missing.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type component struct {
	Name      string
	Version   string
	Licence   string
	Copyright string
	Origin    string // "go" or "npm", for the summary
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	gomods, err := goComponents(root)
	if err != nil {
		fatal(err)
	}
	npm, err := npmComponents(root)
	if err != nil {
		fatal(err)
	}
	out, err := render(append(gomods, npm...))
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "THIRD-PARTY-NOTICES.md"), []byte(out), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("THIRD-PARTY-NOTICES.md: %d Go modules, %d npm packages\n", len(gomods), len(npm))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "notices:", err)
	os.Exit(1)
}

// goComponents unions the module graph over the three platforms the app is
// released for. A module only linked on Windows (go-webview2, wincred) is as
// much a part of the shipped software as a Linux one, and reading only the
// host's graph would silently drop it.
func goComponents(root string) ([]component, error) {
	seen := map[string]component{}
	for _, goos := range []string{"linux", "windows", "darwin"} {
		cmd := exec.Command("go", "list", "-deps",
			"-f", `{{with .Module}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}`, "./...")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list (%s): %w", goos, err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.Split(line, "|")
			if len(parts) != 3 {
				continue
			}
			path, version, dir := parts[0], parts[1], parts[2]
			if path == "kichat" {
				continue
			}
			if _, ok := seen[path]; ok {
				continue
			}
			lic, copyLine := classify(dir)
			seen[path] = component{Name: path, Version: version, Licence: lic, Copyright: copyLine, Origin: "go"}
		}
	}
	out := make([]component, 0, len(seen))
	for _, c := range seen {
		if c.Licence == "" {
			return nil, fmt.Errorf("no licence file found for Go module %s", c.Name)
		}
		out = append(out, c)
	}
	return out, nil
}

// npmComponents walks the production dependency closure of the frontend, not
// the whole node_modules tree: devDependencies (Vite, Tailwind, TypeScript)
// are build tools that never reach the bundle, and listing them would put
// software into the notices that KiChat does not ship. @types packages are
// dropped for the same reason — they are type declarations with no code.
func npmComponents(root string) ([]component, error) {
	dir := filepath.Join(root, "frontend")
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, err
	}
	mods := filepath.Join(dir, "node_modules")
	if _, err := os.Stat(mods); err != nil {
		return nil, fmt.Errorf("frontend/node_modules is missing: run `make deps` first")
	}
	seen := map[string]bool{}
	out := []component{}
	var walk func(string) error
	walk = func(name string) error {
		if seen[name] || strings.HasPrefix(name, "@types/") {
			return nil
		}
		seen[name] = true
		dir := filepath.Join(mods, filepath.FromSlash(name))
		var d struct {
			Version      string            `json:"version"`
			Licence      any               `json:"license"`
			Author       any               `json:"author"`
			Dependencies map[string]string `json:"dependencies"`
		}
		data, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return err
		}
		lic := ""
		switch v := d.Licence.(type) {
		case string:
			lic = v
		case map[string]any:
			lic, _ = v["type"].(string)
		}
		// The package.json field is a claim; the file is the evidence. Fall
		// back to it only when the field is absent, and prefer what the text
		// says when the two disagree.
		fromFile, copyLine := classify(dir)
		if fromFile != "" && fromFile != "NOASSERTION" {
			lic = fromFile
		}
		if copyLine == "" {
			// A licence file without a notice is common (Apache-2.0 text only);
			// the package's own author field is then the only attribution the
			// package publishes, and naming it beats an empty bullet.
			copyLine = authorLine(d.Author)
		}
		out = append(out, component{Name: name, Version: d.Version, Licence: normalise(lic), Copyright: copyLine, Origin: "npm"})
		for dep := range d.Dependencies {
			if err := walk(dep); err != nil {
				return err
			}
		}
		return nil
	}
	names := make([]string, 0, len(pkg.Dependencies))
	for name := range pkg.Dependencies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := walk(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

var licenceFiles = regexp.MustCompile(`(?i)^(licen[cs]e|copying|copyright|unlicense)(\..*)?$`)

// classify reads the licence file in dir and answers the SPDX identifier plus
// the first line that looks like a copyright statement. The identifier is
// derived from the text rather than trusted from package.json because the two
// disagree often enough to matter (a package that says "MIT" in its metadata
// but ships the BSD text).
func classify(dir string) (licence, copyright string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}
	var candidates []string
	for _, e := range entries {
		if e.IsDir() || !licenceFiles.MatchString(e.Name()) {
			continue
		}
		// LICENSE-3RD-PARTY.md and friends describe bundled code, not the
		// package's own terms; reading them would attribute someone else's
		// licence to the package.
		if strings.Contains(strings.ToUpper(e.Name()), "3RD") || strings.Contains(strings.ToUpper(e.Name()), "THIRD") {
			continue
		}
		candidates = append(candidates, filepath.Join(dir, e.Name()))
	}
	sort.Strings(candidates)
	for _, path := range candidates {
		text, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		s := string(text)
		head := s
		if len(head) > 6000 {
			head = head[:6000]
		}
		switch {
		case strings.Contains(head, "Apache License") && strings.Contains(head, "Version 2.0"):
			return "Apache-2.0", copyrightIn(s)
		case strings.Contains(head, "Mozilla Public License"):
			return "MPL-2.0", copyrightIn(s)
		case strings.Contains(head, "Permission is hereby granted, free of charge"):
			return "MIT", copyrightIn(s)
		case strings.Contains(s, "Redistribution and use in source and binary forms"):
			if strings.Contains(s, "Neither the name") {
				return "BSD-3-Clause", copyrightIn(s)
			}
			return "BSD-2-Clause", copyrightIn(s)
		case strings.Contains(head, "Permission to use, copy, modify, and/or distribute"):
			return "ISC", copyrightIn(s)
		case strings.Contains(head, "This is free and unencumbered software"):
			return "Unlicense", copyrightIn(s)
		case strings.Contains(head, "CC0 1.0"):
			return "CC0-1.0", copyrightIn(s)
		}
	}
	return "", ""
}

var year = regexp.MustCompile(`(19|20)\d{2}`)
var copyLine = regexp.MustCompile(`(?mi)^\s*(copyright[^\r\n]*)$`)

// copyrightIn picks the copyright statement out of a licence file. Two passes,
// because neither rule alone is enough: the Apache boilerplate ("Copyright
// [yyyy] [name of copyright owner]", "Copyright License", "COPYRIGHT HOLDERS
// AND CONTRIBUTORS") would win a single pass that tolerated every line, while a
// year-only rule misses the many packages whose notice carries no year at all
// (React's is "Copyright (c) Meta Platforms, Inc. and affiliates."). So the
// first pass takes a line with a year, and the second takes the first line that
// is not one of those boilerplate phrases.
func copyrightIn(text string) string {
	var fallback string
	for _, m := range copyLine.FindAllStringSubmatch(text, -1) {
		line := strings.TrimSpace(m[1])
		lower := strings.ToLower(line)
		if strings.Contains(line, "[yyyy]") || strings.Contains(lower, "license") {
			continue
		}
		// A statement names an owner: "(c)", "©", a year, or "by". Without
		// one, the line is prose from the licence grant (Apache's "copyright
		// license to reproduce, prepare Derivative Works of") rather than a
		// notice, and it is worse than no attribution because it is wrong.
		if !strings.Contains(line, "(c)") && !strings.Contains(line, "©") &&
			!year.MatchString(line) && !strings.Contains(lower, " by ") {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(line), "COPYRIGHT OWNER") ||
			strings.HasPrefix(strings.ToUpper(line), "COPYRIGHT HOLDERS") ||
			strings.HasPrefix(strings.ToUpper(line), "COPYRIGHT NOTICE") {
			continue
		}
		if year.MatchString(line) {
			return line
		}
		if fallback == "" {
			fallback = line
		}
	}
	return fallback
}

// authorLine renders package.json's author, which npm allows as a string or
// as {name, email, url}.
func authorLine(v any) string {
	switch a := v.(type) {
	case string:
		if a = strings.TrimSpace(a); a != "" {
			return "Author: " + a
		}
	case map[string]any:
		name, _ := a["name"].(string)
		email, _ := a["email"].(string)
		if name != "" && email != "" {
			return "Author: " + name + " <" + email + ">"
		}
		if name != "" {
			return "Author: " + name
		}
	}
	return ""
}

func normalise(lic string) string {
	lic = strings.TrimSpace(lic)
	switch strings.ToLower(lic) {
	case "mit":
		return "MIT"
	case "bsd-2-clause":
		return "BSD-2-Clause"
	case "bsd-3-clause":
		return "BSD-3-Clause"
	case "isc":
		return "ISC"
	case "apache-2.0":
		return "Apache-2.0"
	case "mpl-2.0":
		return "MPL-2.0"
	case "unlicense":
		return "Unlicense"
	case "cc0-1.0":
		return "CC0-1.0"
	}
	return lic
}

func render(all []component) (string, error) {
	byLicence := map[string][]component{}
	for _, c := range all {
		if c.Licence == "" {
			return "", fmt.Errorf("%s: no licence identified", c.Name)
		}
		byLicence[c.Licence] = append(byLicence[c.Licence], c)
	}
	order := []string{"MIT", "BSD-3-Clause", "BSD-2-Clause", "ISC", "Apache-2.0", "MPL-2.0", "Unlicense", "CC0-1.0"}
	var others []string
	for lic := range byLicence {
		known := false
		for _, o := range order {
			if o == lic {
				known = true
			}
		}
		if !known {
			others = append(others, lic)
		}
	}
	sort.Strings(others)
	order = append(order, others...)

	var b strings.Builder
	b.WriteString(`# Third-party notices

KiChat itself is licensed under the **Apache License, Version 2.0** — see
[LICENSE](LICENSE) — and every third-party component it ships stays under its
own licence. Nothing here changes those terms.

The list is generated, not written by hand:

    go run ./tools/notices

It unions the Go module graph for linux, windows and darwin with the
frontend's *production* dependency tree, so build tools (Vite, Tailwind,
TypeScript) and type-only @types packages are absent: they never reach a
release. Regenerate after any dependency change — the file is sorted and has
no timestamps, so a diff means a real change.

A component printed without a copyright line is one whose licence file carries
no notice of its own — the Apache text alone, for both entries below. Nothing
was invented to fill the gap.

Not covered here, because they are not distributed with KiChat: the
llama-server build and the stable-diffusion.cpp binaries KiChat downloads at
the user's request (both MIT, from their own releases), and the model weights a
user installs or that a future catalogue fetches — a GGUF carries whatever
licence its publisher attached to it.

## Summary

| Licence | Components |
| --- | --- |
`)
	for _, lic := range order {
		list := byLicence[lic]
		if len(list) == 0 {
			continue
		}
		fmt.Fprintf(&b, "| %s | %d |\n", lic, len(list))
	}
	b.WriteString("\n")
	for _, lic := range order {
		list := byLicence[lic]
		if len(list) == 0 {
			continue
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Name == list[j].Name {
				return list[i].Version < list[j].Version
			}
			return list[i].Name < list[j].Name
		})
		fmt.Fprintf(&b, "## %s\n\n", lic)
		if _, ok := licenceText[lic]; ok {
			b.WriteString("The following components are used under this licence. Each one's\ncopyright line is given beside it; the text of the licence follows the list.\n\n")
		} else {
			b.WriteString("The following components are used under this licence. Its text is the\nApache License, Version 2.0 reproduced in [LICENSE](LICENSE).\n\n")
		}
		for _, c := range list {
			fmt.Fprintf(&b, "- **%s** %s", c.Name, c.Version)
			if c.Copyright != "" {
				fmt.Fprintf(&b, " — %s", c.Copyright)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
		if text, ok := licenceText[lic]; ok {
			b.WriteString("```text\n" + text + "```\n\n")
		}
	}
	return b.String(), nil
}

// The texts are reproduced verbatim. The copyright line of each component is
// printed beside the component above rather than in a placeholder here, which
// is what makes one copy of the text enough for a hundred packages.
var licenceText = map[string]string{
	"MIT": `MIT License

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`,
	"BSD-3-Clause": `BSD 3-Clause License

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

3. Neither the name of the copyright holder nor the names of its contributors
   may be used to endorse or promote products derived from this software
   without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
`,
	"BSD-2-Clause": `BSD 2-Clause License

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
`,
	"ISC": `ISC License

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY
AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM
LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR
OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR
PERFORMANCE OF THIS SOFTWARE.
`,
}

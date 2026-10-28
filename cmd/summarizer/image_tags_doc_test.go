package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// goreleaserConfig is the part of .goreleaser.yaml that decides which
// container tags exist.
type goreleaserConfig struct {
	DockersV2 []struct {
		ID     string   `yaml:"id"`
		Images []string `yaml:"images"`
		Tags   []string `yaml:"tags"`
	} `yaml:"dockers_v2"`
}

// publishedImageTags returns every tag the release will push, with
// goreleaser's `{{ .Version }}` left as a placeholder — goreleaser
// renders it as the version WITHOUT a leading "v", so 0.3.0, not
// v0.3.0.
func publishedImageTags(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yaml: %v", err)
	}
	var cfg goreleaserConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}
	tags := map[string]bool{}
	for _, d := range cfg.DockersV2 {
		for _, tag := range d.Tags {
			tags[strings.ReplaceAll(tag, "{{ .Version }}", "VERSION")] = true
		}
	}
	if len(tags) == 0 {
		t.Fatal(".goreleaser.yaml has no dockers_v2 tags")
	}
	return tags
}

// TestDocs_ImageTagsExist: the README told operators to
// `docker pull ghcr.io/inful/summarizer:debug`, and the debug
// Dockerfile told them to run `:debug /busybox/sh` for a shell.
//
// Neither was true. The release publishes `{{ .Version }}` and `latest`
// for the production image and `{{ .Version }}-debug` and `latest-debug`
// for the debug one — there has never been a plain `:debug` tag:
//
//	docker pull ghcr.io/inful/summarizer:debug
//	Error response from daemon: manifest unknown
//
// and the second half was wrong too: the image's entrypoint is the
// binary, so `/busybox/sh` arrives as an ARGUMENT to summarizer and
// the binary prints its usage instead of giving a shell.
//
// Every image reference in the docs must name a tag the release
// publishes. Nothing else checked it, which is how the command survived
// two releases.
func TestDocs_ImageTagsExist(t *testing.T) {
	published := publishedImageTags(t)

	ref := regexp.MustCompile(`ghcr\.io/inful/summarizer:([A-Za-z0-9._-]+)`)
	for _, doc := range []string{
		filepath.Join("..", "..", "README.md"),
		filepath.Join("..", "..", "Dockerfile"),
		filepath.Join("..", "..", "Dockerfile.debug"),
	} {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for _, m := range ref.FindAllStringSubmatch(string(b), -1) {
			tag := m[1]
			if !published[tag] && !strings.Contains(tag, "VERSION") {
				t.Errorf("%s references ghcr.io/inful/summarizer:%s, which the release "+
					"does not publish (it publishes %v)", doc, tag, keys(published))
			}
		}
	}
}

// TestDocs_DebugImageInvocationUsesEntrypoint: `/busybox/sh` as an
// argument to the image does not give a shell, because the entrypoint
// is the binary. The documented command has to override it.
func TestDocs_DebugImageInvocationUsesEntrypoint(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile.debug"))
	if err != nil {
		t.Fatalf("read Dockerfile.debug: %v", err)
	}
	doc := string(b)
	i := strings.Index(doc, "/busybox/sh")
	if i < 0 {
		t.Fatal("Dockerfile.debug no longer mentions /busybox/sh; this test is stale")
	}
	// The line before the mention must not be the bare image path.
	line := doc[:i]
	if j := strings.LastIndex(line, "\n"); j >= 0 {
		line = line[j:]
	}
	if strings.Contains(line, "/busybox/sh") && !strings.Contains(line, "--entrypoint") {
		t.Errorf("Dockerfile.debug documents a /busybox/sh invocation without "+
			"--entrypoint; the image's entrypoint is the binary, so the shell "+
			"never runs:\n%s", line)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

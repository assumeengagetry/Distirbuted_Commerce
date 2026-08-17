package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var digestPattern = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

func main() {
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: release-verify RENDERED_MANIFEST.yaml")
		os.Exit(2)
	}
	registry := os.Getenv("RELEASE_IMAGE_REGISTRY")
	if registry == "" {
		registry = "ghcr.io/assumeengagetry/distributed-commerce"
	}
	file, err := os.Open(os.Args[1])
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "open release manifest")
		os.Exit(1)
	}
	defer file.Close()
	count, err := verifyManifest(file, registry)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_, _ = fmt.Fprintf(os.Stdout, "verified %d immutable application images\n", count)
}

func verifyManifest(reader io.Reader, registry string) (int, error) {
	if strings.TrimSpace(registry) == "" || strings.ContainsAny(registry, "\r\n \t") {
		return 0, fmt.Errorf("release image registry is invalid")
	}
	decoder := yaml.NewDecoder(reader)
	seen := make(map[string]struct{})
	images := 0
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("decode rendered manifest: %w", err)
		}
		if document.Kind == 0 {
			continue
		}
		if err := inspectNode(&document, registry, seen, &images); err != nil {
			return 0, err
		}
	}
	if images < 5 {
		return 0, fmt.Errorf("release manifest contains %d application images, want at least 5", images)
	}
	return images, nil
}

func inspectNode(node *yaml.Node, registry string, seen map[string]struct{}, images *int) error {
	if node.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Value == "kind" && value.Value == "Secret" {
				return fmt.Errorf("release manifest must not contain Secret resources")
			}
			if key.Value == "image" && value.Kind == yaml.ScalarNode {
				if err := verifyImage(value.Value, registry); err != nil {
					return err
				}
				if _, duplicate := seen[value.Value]; duplicate {
					return fmt.Errorf("release manifest contains a duplicate image %q", value.Value)
				}
				seen[value.Value] = struct{}{}
				*images++
			}
			if err := inspectNode(value, registry, seen, images); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range node.Content {
		if err := inspectNode(child, registry, seen, images); err != nil {
			return err
		}
	}
	return nil
}

func verifyImage(image, registry string) error {
	if strings.Contains(image, ".invalid") || strings.Contains(image, ":phase8") || strings.Contains(image, ":dev") {
		return fmt.Errorf("release image contains a placeholder tag or host: %s", image)
	}
	if !strings.HasPrefix(image, registry+"/") {
		return fmt.Errorf("release image is outside the approved registry: %s", image)
	}
	if !digestPattern.MatchString(image) {
		return fmt.Errorf("release image is not pinned by a sha256 digest: %s", image)
	}
	return nil
}

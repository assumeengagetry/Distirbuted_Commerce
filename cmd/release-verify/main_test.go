package main

import (
	"strings"
	"testing"
)

func TestVerifyManifestRequiresApprovedImmutableImages(t *testing.T) {
	t.Parallel()
	valid := strings.NewReader(`
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - image: ghcr.io/assumeengagetry/distributed-commerce/user-service@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        - image: ghcr.io/assumeengagetry/distributed-commerce/order-service@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
        - image: ghcr.io/assumeengagetry/distributed-commerce/payment-service@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
        - image: ghcr.io/assumeengagetry/distributed-commerce/job-worker@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
        - image: ghcr.io/assumeengagetry/distributed-commerce/migrator@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
`)
	if count, err := verifyManifest(valid, "ghcr.io/assumeengagetry/distributed-commerce"); err != nil || count != 5 {
		t.Fatalf("verifyManifest(valid) = (%d, %v)", count, err)
	}
	for _, manifest := range []string{
		"kind: Secret\n",
		"kind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n        - image: registry.example.invalid/app:phase8\n",
	} {
		if _, err := verifyManifest(strings.NewReader(manifest), "ghcr.io/assumeengagetry/distributed-commerce"); err == nil {
			t.Errorf("verifyManifest(%q) error = nil", manifest)
		}
	}
}

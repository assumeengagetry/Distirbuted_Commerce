package openapi_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

const (
	userServerURL    = "http://127.0.0.1:8081"
	orderServerURL   = "http://127.0.0.1:8082"
	paymentServerURL = "http://127.0.0.1:8083"
)

func TestCommerceOpenAPI(t *testing.T) {
	t.Parallel()

	loader := openapi3.NewLoader()
	document, err := loader.LoadFromFile("commerce.yaml")
	if err != nil {
		t.Fatalf("load commerce.yaml: %v", err)
	}
	if document.OpenAPI != "3.1.0" {
		t.Errorf("OpenAPI version = %q, want 3.1.0", document.OpenAPI)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate commerce.yaml: %v", err)
	}

	expected := expectedOperations()
	expectedPaths := make(map[string]struct{})
	for key := range expected {
		_, path, _ := strings.Cut(key, " ")
		expectedPaths[path] = struct{}{}
	}
	if document.Paths.Len() != len(expectedPaths) {
		t.Errorf("path count = %d, want %d", document.Paths.Len(), len(expectedPaths))
	}
	actual := make(map[string]*openapi3.Operation, len(expected))
	for path, pathItem := range document.Paths.Map() {
		if _, ok := expectedPaths[path]; !ok {
			t.Errorf("unexpected path %s", path)
		}
		for method, operation := range pathItem.Operations() {
			key := operationKey(method, path)
			actual[key] = operation
			assertResponsesDefined(t, key, operation)
		}
	}
	if len(actual) != len(expected) {
		t.Errorf("operation count = %d, want %d", len(actual), len(expected))
	}
	for key := range actual {
		if _, ok := expected[key]; !ok {
			t.Errorf("unexpected operation %s", key)
		}
	}
	for key, contract := range expected {
		operation, ok := actual[key]
		if !ok {
			t.Errorf("missing operation %s", key)
			continue
		}
		assertOperationServers(t, key, operation, contract.servers)
		assertOperationSecurity(t, key, operation, contract.protected)
	}
}

type operationContract struct {
	servers   []string
	protected bool
}

func expectedOperations() map[string]operationContract {
	probeServers := []string{userServerURL, orderServerURL, paymentServerURL}
	return map[string]operationContract{
		"GET /healthz":                           {servers: probeServers},
		"GET /readyz":                            {servers: probeServers},
		"POST /v1/auth/register":                 {servers: []string{userServerURL}},
		"POST /v1/auth/login":                    {servers: []string{userServerURL}},
		"POST /v1/auth/refresh":                  {servers: []string{userServerURL}},
		"POST /v1/auth/logout":                   {servers: []string{userServerURL}},
		"GET /v1/users/me":                       {servers: []string{userServerURL}, protected: true},
		"GET /v1/products":                       {servers: []string{orderServerURL}},
		"GET /v1/products/{product_id}":          {servers: []string{orderServerURL}},
		"POST /v1/admin/products":                {servers: []string{orderServerURL}, protected: true},
		"GET /v1/admin/products/{product_id}":    {servers: []string{orderServerURL}, protected: true},
		"PATCH /v1/admin/products/{product_id}":  {servers: []string{orderServerURL}, protected: true},
		"GET /v1/admin/inventory":                {servers: []string{orderServerURL}, protected: true},
		"PATCH /v1/admin/inventory/{product_id}": {servers: []string{orderServerURL}, protected: true},
		"POST /v1/orders":                        {servers: []string{orderServerURL}, protected: true},
		"GET /v1/orders":                         {servers: []string{orderServerURL}, protected: true},
		"GET /v1/orders/{order_id}":              {servers: []string{orderServerURL}, protected: true},
		"POST /v1/payments":                      {servers: []string{paymentServerURL}, protected: true},
		"GET /v1/payments/{payment_id}":          {servers: []string{paymentServerURL}, protected: true},
	}
}

func operationKey(method, path string) string {
	return strings.ToUpper(method) + " " + path
}

func assertOperationServers(t *testing.T, key string, operation *openapi3.Operation, want []string) {
	t.Helper()
	if operation.Servers == nil {
		t.Errorf("%s has no operation-level servers", key)
		return
	}
	got := make([]string, 0, len(*operation.Servers))
	for _, server := range *operation.Servers {
		got = append(got, server.URL)
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s servers = %v, want %v", key, got, want)
	}
}

func assertOperationSecurity(t *testing.T, key string, operation *openapi3.Operation, protected bool) {
	t.Helper()
	if !protected {
		return
	}
	if operation.Security == nil {
		t.Errorf("%s has no operation-level security", key)
		return
	}
	for _, requirement := range *operation.Security {
		if scopes, ok := requirement["bearerAuth"]; ok && len(scopes) == 0 {
			return
		}
	}
	t.Errorf("%s does not require bearerAuth", key)
}

func assertResponsesDefined(t *testing.T, key string, operation *openapi3.Operation) {
	t.Helper()
	if operation.Responses == nil || operation.Responses.Len() == 0 {
		t.Errorf("%s has no responses", key)
		return
	}
	for status, responseRef := range operation.Responses.Map() {
		if responseRef == nil || responseRef.Value == nil {
			t.Errorf("%s response %s is unresolved", key, status)
			continue
		}
		if responseRef.Value.Description == nil || strings.TrimSpace(*responseRef.Value.Description) == "" {
			t.Errorf("%s response %s has no description", key, status)
		}
		if status == "default" {
			t.Errorf("%s uses an unspecified default response", key)
		}
		if len(status) != 3 {
			t.Errorf("%s has invalid response status %q", key, status)
		}
	}
}

package app_test

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"placementhub/internal/testutil"

	"github.com/go-chi/chi/v5"
)

// The API contract in openapi.yaml must list exactly the routes the server serves.
func TestOpenAPIDocumentsExactlyTheRegisteredRoutes(t *testing.T) {
	e := testutil.NewEnv(t)

	served := map[string]bool{}
	err := chi.Walk(e.App.Handler.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.ReplaceAll(route, "/*", "")
		if len(route) > 1 {
			route = strings.TrimRight(route, "/")
		}
		served[strings.ToLower(method)+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	pathRE := regexp.MustCompile(`^  (/[^\s:]*):\s*$`)
	methodRE := regexp.MustCompile(`^    (get|post|put|patch|delete):`)
	current := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if m := pathRE.FindStringSubmatch(line); m != nil {
			current = m[1]
		} else if m := methodRE.FindStringSubmatch(line); m != nil && current != "" {
			documented[m[1]+" "+current] = true
		} else if strings.HasPrefix(line, "components:") {
			current = ""
		}
	}

	var missingDocs, missingRoutes []string
	for r := range served {
		if !documented[r] {
			missingDocs = append(missingDocs, r)
		}
	}
	for d := range documented {
		if !served[d] {
			missingRoutes = append(missingRoutes, d)
		}
	}
	sort.Strings(missingDocs)
	sort.Strings(missingRoutes)
	if len(missingDocs) > 0 {
		t.Errorf("routes served but not in openapi.yaml:\n  %s", strings.Join(missingDocs, "\n  "))
	}
	if len(missingRoutes) > 0 {
		t.Errorf("documented in openapi.yaml but not served:\n  %s", strings.Join(missingRoutes, "\n  "))
	}
	if len(documented) < 80 {
		t.Errorf("only %d operations parsed from the spec; the parser or the file is broken", len(documented))
	}
}

package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

func TestFetchJSONScopesCredentialsAndSanitizesErrors(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "/fail":
			http.Error(w, `{"error":{"message":"secret detail"}}`, http.StatusUnauthorized)
		default:
			if r.Header.Get("X-Test") != "set" || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("headers: %v", r.Header)
			}
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer server.Close()
	headers := func(r *http.Request) { r.Header.Set("X-Test", "set") }
	raw, err := FetchJSON(context.Background(), server.Client(), http.MethodGet, server.URL+"/ok", nil, headers)
	if err != nil || raw["ok"] != true {
		t.Fatalf("fetch: %v %v", raw, err)
	}
	if _, err := FetchJSON(context.Background(), server.Client(), http.MethodGet, server.URL+"/redirect", nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirect followed: %v", err)
	}
	if _, err := FetchJSON(context.Background(), server.Client(), http.MethodGet, server.URL+"/fail", nil, nil); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error body leaked: %v", err)
	}
	if hits.Load() != 3 {
		t.Fatalf("requests = %d, want 3 (no redirect target fetched)", hits.Load())
	}
}

func TestWalkStopsOnRepeatedTokenAndPageCap(t *testing.T) {
	pages := 0
	err := Walk(func(token string) (string, error) {
		pages++
		return "same", nil
	})
	if err == nil || pages != 2 {
		t.Fatalf("cycle: %v after %d pages", err, pages)
	}
	pages = 0
	err = Walk(func(token string) (string, error) {
		pages++
		return fmt.Sprint(pages), nil
	})
	if err == nil || pages != maxPages {
		t.Fatalf("cap: %v after %d pages", err, pages)
	}
	pages = 0
	if err := Walk(func(string) (string, error) { pages++; return "", nil }); err != nil || pages != 1 {
		t.Fatalf("single page: %v %d", err, pages)
	}
}

func TestNextPageRequiresLastID(t *testing.T) {
	if next, err := NextPage(map[string]any{"nextPageToken": "n"}); err != nil || next != "n" {
		t.Fatalf("token: %q %v", next, err)
	}
	if next, err := NextPage(map[string]any{"has_more": true, "last_id": "id"}); err != nil || next != "id" {
		t.Fatalf("cursor: %q %v", next, err)
	}
	if _, err := NextPage(map[string]any{"has_more": true}); err == nil {
		t.Fatal("has_more without last_id accepted")
	}
}

func TestListingDedupesFiltersAndReportsUnknown(t *testing.T) {
	var l Listing
	page := map[string]any{"data": []any{map[string]any{"id": "b"}, map[string]any{"id": "a"}, map[string]any{"id": "a"}, "junk"}}
	if err := l.AddPage(page, "data", "", BaseModel); err != nil {
		t.Fatal(err)
	}
	cat, err := l.Catalog("", nil)
	if err != nil || len(cat.Models) != 2 || cat.Models[0].ID != "a" || !cat.Partial {
		t.Fatalf("listing: %+v %v", cat, err)
	}

	var named Listing
	if err := named.AddPage(page, "data", "b", BaseModel); err != nil {
		t.Fatal(err)
	}
	if cat, err := named.Catalog("b", nil); err != nil || len(cat.Models) != 1 || cat.Models[0].ID != "b" {
		t.Fatalf("named: %+v %v", cat, err)
	}

	var single Listing
	if err := single.AddPage(map[string]any{"id": "canonical"}, "data", "alias", BaseModel); err != nil {
		t.Fatal(err)
	}
	if cat, err := single.Catalog("alias", nil); err != nil || len(cat.Models) != 1 || cat.Models[0].ID != "canonical" {
		t.Fatalf("single object: %+v %v", cat, err)
	}

	var empty Listing
	if err := empty.AddPage(map[string]any{}, "data", "", BaseModel); !errors.Is(err, contract.ErrModelMetadataUnknown) {
		t.Fatalf("no rows: %v", err)
	}
	if _, err := empty.Catalog("missing", nil); !errors.Is(err, contract.ErrModelMetadataUnknown) {
		t.Fatalf("missing model: %v", err)
	}
	failed := errors.New("page failed")
	if cat, err := l.Catalog("", failed); !errors.Is(err, failed) || !cat.Partial || len(cat.Models) != 2 {
		t.Fatalf("failure keeps collected rows: %+v %v", cat, err)
	}
}

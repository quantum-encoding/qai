package joplin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestSplitFolderPath(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"qai", []string{"qai"}},
		{"qai/scans", []string{"qai", "scans"}},
		{"qai/scans/dag-cli", []string{"qai", "scans", "dag-cli"}},
		{"  qai / scans / dag-cli ", []string{"qai", "scans", "dag-cli"}},
		{"/qai/scans/", []string{"qai", "scans"}},
		{"qai//scans", []string{"qai", "scans"}},
		{"", nil},
		{"   /   /  ", nil},
	}
	for _, tt := range tests {
		got := SplitFolderPath(tt.in)
		if len(got) == 0 && len(tt.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitFolderPath(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// fakeJoplin spins up a minimal HTTP stand-in for Joplin's folder API so
// FindOrCreateFolderPath can be tested without a live Joplin instance.
type fakeJoplin struct {
	mu      atomic.Pointer[[]Folder]
	nextID  atomic.Int64
	creates atomic.Int64 // POSTs observed; lets us assert "no extra creates".
}

func newFakeJoplin(initial []Folder) *fakeJoplin {
	f := &fakeJoplin{}
	cp := append([]Folder(nil), initial...)
	f.mu.Store(&cp)
	return f
}

func (f *fakeJoplin) folders() []Folder {
	return *f.mu.Load()
}

func (f *fakeJoplin) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/folders":
			_ = json.NewEncoder(w).Encode(folderList{Items: f.folders()})
		case r.Method == http.MethodPost && r.URL.Path == "/folders":
			var body struct {
				Title    string `json:"title"`
				ParentID string `json:"parent_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			id := atomicID(&f.nextID)
			created := Folder{ID: id, Title: body.Title, ParentID: body.ParentID}
			next := append(f.folders(), created)
			f.mu.Store(&next)
			f.creates.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(created)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}
}

func atomicID(c *atomic.Int64) string {
	n := c.Add(1)
	return "fake-id-" + itoa(n)
}

func itoa(n int64) string {
	// Avoid importing strconv just for tests; tiny non-negative ints only.
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func newTestClient(srv *httptest.Server) *Client {
	return New(Config{BaseURL: srv.URL, Token: "test-token"})
}

func TestFindOrCreateFolderPath_CreatesChainWhenMissing(t *testing.T) {
	fake := newFakeJoplin(nil)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(srv)

	leaf, err := c.FindOrCreateFolderPath("qai/scans/dag-cli")
	if err != nil {
		t.Fatalf("FindOrCreateFolderPath: %v", err)
	}
	if leaf == nil || leaf.Title != "dag-cli" {
		t.Fatalf("leaf = %+v, want title dag-cli", leaf)
	}
	if got := fake.creates.Load(); got != 3 {
		t.Errorf("creates = %d, want 3 (one per segment)", got)
	}
	// Walk up via ParentID and confirm the chain shape.
	all := fake.folders()
	byID := map[string]Folder{}
	for _, f := range all {
		byID[f.ID] = f
	}
	if leaf.ParentID == "" || byID[leaf.ParentID].Title != "scans" {
		t.Errorf("leaf parent = %q (%q), want scans", leaf.ParentID, byID[leaf.ParentID].Title)
	}
	mid := byID[leaf.ParentID]
	if mid.ParentID == "" || byID[mid.ParentID].Title != "qai" {
		t.Errorf("mid parent = %q (%q), want qai", mid.ParentID, byID[mid.ParentID].Title)
	}
	root := byID[mid.ParentID]
	if root.ParentID != "" {
		t.Errorf("root.ParentID = %q, want empty", root.ParentID)
	}
}

func TestFindOrCreateFolderPath_ReusesExistingChain(t *testing.T) {
	// Pre-seed the full chain. Calling FindOrCreateFolderPath should not
	// create anything.
	fake := newFakeJoplin([]Folder{
		{ID: "root-1", Title: "qai", ParentID: ""},
		{ID: "mid-2", Title: "scans", ParentID: "root-1"},
		{ID: "leaf-3", Title: "dag-cli", ParentID: "mid-2"},
	})
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(srv)

	leaf, err := c.FindOrCreateFolderPath("qai/scans/dag-cli")
	if err != nil {
		t.Fatalf("FindOrCreateFolderPath: %v", err)
	}
	if leaf.ID != "leaf-3" {
		t.Errorf("leaf = %+v, want pre-existing id leaf-3", leaf)
	}
	if got := fake.creates.Load(); got != 0 {
		t.Errorf("creates = %d, want 0 (full chain pre-existed)", got)
	}
}

func TestFindOrCreateFolderPath_PartialChainExtends(t *testing.T) {
	// "qai" exists at root; "scans" needs creating under it; "dag-cli" too.
	fake := newFakeJoplin([]Folder{
		{ID: "root-1", Title: "qai", ParentID: ""},
	})
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(srv)

	leaf, err := c.FindOrCreateFolderPath("qai/scans/dag-cli")
	if err != nil {
		t.Fatalf("FindOrCreateFolderPath: %v", err)
	}
	if leaf.Title != "dag-cli" {
		t.Errorf("leaf title = %q, want dag-cli", leaf.Title)
	}
	if got := fake.creates.Load(); got != 2 {
		t.Errorf("creates = %d, want 2 (scans + dag-cli)", got)
	}
}

func TestFindOrCreateFolderPath_DistinguishesByParent(t *testing.T) {
	// Two folders titled "scans" — one under "qai", one under "other".
	// FindOrCreateFolderPath must walk by parent, not by title alone.
	fake := newFakeJoplin([]Folder{
		{ID: "root-1", Title: "qai", ParentID: ""},
		{ID: "root-2", Title: "other", ParentID: ""},
		{ID: "mid-3", Title: "scans", ParentID: "root-2"},
	})
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	c := newTestClient(srv)

	leaf, err := c.FindOrCreateFolderPath("qai/scans")
	if err != nil {
		t.Fatalf("FindOrCreateFolderPath: %v", err)
	}
	if leaf.ParentID != "root-1" {
		t.Errorf("leaf.ParentID = %q, want root-1 (qai)", leaf.ParentID)
	}
	if got := fake.creates.Load(); got != 1 {
		t.Errorf("creates = %d, want 1 (new scans under qai)", got)
	}
}

func TestFindOrCreateFolderPath_RejectsEmptyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("FindOrCreateFolderPath should not hit the network on empty input")
	}))
	defer srv.Close()
	c := newTestClient(srv)

	if _, err := c.FindOrCreateFolderPath("   "); err == nil {
		t.Errorf("FindOrCreateFolderPath(\"   \") returned nil error, want non-nil")
	}
}

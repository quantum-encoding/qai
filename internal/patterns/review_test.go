package patterns

// review_test.go — table tests for the diff parser + matchCandidates
// against the 9 fixtures from 31-diff-parser-fixtures.md.
//
// Critical-path coverage. Run with `go test ./internal/patterns/...`.

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// parser fixtures (1–6, 9)
// ---------------------------------------------------------------------------

func TestParseUnifiedDiff(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []diffFile
	}{
		// -----------------------------------------------------------------
		// 1. Header lines must not count as added text.
		{
			name: "fixture 1: +++ b/path and @@ headers excluded from Added",
			input: `diff --git a/internal/net/http_default_client_notes.go b/internal/net/http_default_client_notes.go
index 1111111..2222222 100644
--- a/internal/net/http_default_client_notes.go
+++ b/internal/net/http_default_client_notes.go
@@ -1,2 +1,2 @@
 package net
-// old comment
+// new comment
`,
			want: []diffFile{
				{
					Path:  "internal/net/http_default_client_notes.go",
					Added: "// new comment\n",
				},
			},
		},

		// -----------------------------------------------------------------
		// 2. New file (/dev/null → file): every body line is added.
		{
			name: "fixture 2: new file via /dev/null source",
			input: `diff --git a/internal/server/routes_pay.go b/internal/server/routes_pay.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/internal/server/routes_pay.go
@@ -0,0 +1,3 @@
+package server
+
+func payHandler(w http.ResponseWriter, r *http.Request) { charge(r) }
`,
			want: []diffFile{
				{
					Path:  "internal/server/routes_pay.go",
					Added: "package server\n\nfunc payHandler(w http.ResponseWriter, r *http.Request) { charge(r) }\n",
				},
			},
		},

		// -----------------------------------------------------------------
		// 3. Deletion (file → /dev/null): file is dropped entirely.
		{
			name: "fixture 3: deletion produces no entry",
			input: `diff --git a/internal/legacy/old.go b/internal/legacy/old.go
deleted file mode 100644
index 4444444..0000000
--- a/internal/legacy/old.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package legacy
-func gone() {}
`,
			want: nil,
		},

		// -----------------------------------------------------------------
		// 4. Rename with no content change: no hunk, no entry.
		{
			name: "fixture 4: hunkless rename produces no entry",
			input: `diff --git a/internal/old_name.go b/internal/new_name.go
similarity index 100%
rename from internal/old_name.go
rename to internal/new_name.go
`,
			want: nil,
		},

		// -----------------------------------------------------------------
		// 5. Binary file change: skipped cleanly.
		{
			name: "fixture 5: binary diff skipped cleanly",
			input: `diff --git a/assets/logo.png b/assets/logo.png
index 5555555..6666666 100644
Binary files a/assets/logo.png and b/assets/logo.png differ
`,
			want: nil,
		},

		// -----------------------------------------------------------------
		// 6. Multi-file, multi-hunk: additions attributed to the right file.
		{
			name: "fixture 6: multi-file additions don't bleed across boundary",
			input: `diff --git a/internal/a/handler.go b/internal/a/handler.go
index aaa..bbb 100644
--- a/internal/a/handler.go
+++ b/internal/a/handler.go
@@ -10,1 +10,2 @@
 func A() {}
+func fetchA() { http.Get(userURL) }
diff --git a/internal/b/store.go b/internal/b/store.go
index ccc..ddd 100644
--- a/internal/b/store.go
+++ b/internal/b/store.go
@@ -5,1 +5,2 @@
 func B() {}
+db.ExecRaw(userInput)
`,
			want: []diffFile{
				{
					Path:  "internal/a/handler.go",
					Added: "func fetchA() { http.Get(userURL) }\n",
				},
				{
					Path:  "internal/b/store.go",
					Added: "db.ExecRaw(userInput)\n",
				},
			},
		},

		// -----------------------------------------------------------------
		// 9. Quoted path with a space (git's C-quoted special-char paths).
		{
			name: "fixture 9: quoted path with space",
			input: `diff --git "a/internal/has space.go" "b/internal/has space.go"
index bbbbbbb..ccccccc 100644
--- "a/internal/has space.go"
+++ "b/internal/has space.go"
@@ -1,1 +1,2 @@
 package x
+var y = 1
`,
			want: []diffFile{
				{
					Path:  "internal/has space.go",
					Added: "var y = 1\n",
				},
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := parseUnifiedDiff(strings.NewReader(tc.input))
			if len(got) != len(tc.want) {
				t.Fatalf("len(files)=%d, want %d\n  got=%+v\n want=%+v", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i].Path != tc.want[i].Path {
					t.Errorf("file %d: path=%q want %q", i, got[i].Path, tc.want[i].Path)
				}
				if got[i].Added != tc.want[i].Added {
					t.Errorf("file %d: added=%q\n         want %q", i, got[i].Added, tc.want[i].Added)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// matchCandidates fixtures (7, 8) + a smell+role baseline
// ---------------------------------------------------------------------------

// minimalPools constructs the three pattern pools that mirror what fetchPools
// would return from a populated DB. Just enough rows to exercise the
// fixtures — keep tight; the real corpus lives in the DB.
func minimalPools() (smellByTrigger, smellByRole, control []patternForReview) {
	metadataSSRF := patternForReview{
		Slug: "metadata_ssrf", Name: "Cloud metadata SSRF", Category: "ssrf",
		Polarity:       "smell",
		Triggers:       []string{"http.DefaultClient", "http.NewRequest", "169.254.169.254"},
		AppliesToRoles: []string{"handler", "sdk_client", "data_access"},
		Detection:      "user URL fetched without IP blocklist?",
	}
	rawExec := patternForReview{
		Slug: "surrealql_raw_exec", Name: "Raw SurrealQL execution", Category: "injection",
		Polarity:       "smell",
		Triggers:       []string{"db.ExecRaw", "ExecRaw("},
		AppliesToRoles: []string{"handler", "data_access"},
	}
	unrateLimited := patternForReview{
		Slug: "unauthenticated_endpoints_unlimited", Name: "Unrate-limited public endpoints",
		Category: "rate_limit", Polarity: "control",
		Triggers:       []string{"httprate.Limit", "rate.NewLimiter", "golang.org/x/time/rate"},
		AppliesToRoles: []string{"handler", "middleware"},
	}
	smellByTrigger = []patternForReview{metadataSSRF, rawExec}
	smellByRole = []patternForReview{metadataSSRF, rawExec} // same as above for these fixtures
	control = []patternForReview{unrateLimited}
	return
}

// Helper: was a slug raised with a given kind?
func hasCandidate(cands []Candidate, slug, kind string) bool {
	for _, c := range cands {
		if c.Slug == slug && c.Kind == kind {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------
// Fixture 7: trigger in a comment is correctly raised as a candidate.
// (High-recall is by design; the LLM does precision.)
func TestMatchCandidates_Fixture7_TriggerInComment(t *testing.T) {
	smellByTrigger, smellByRole, control := minimalPools()
	added := "// NOTE: we deliberately avoid http.DefaultClient and disable redirects here\n"

	cands := matchCandidates("handler", added, "", false, smellByTrigger, smellByRole, control)

	if !hasCandidate(cands, "metadata_ssrf", "smell") {
		t.Errorf("expected metadata_ssrf as 'smell' candidate (high-recall over comments is intentional). got=%+v", cands)
	}
}

// -----------------------------------------------------------------
// Fixture 8: the load-bearing one. A handler is added in the diff but the
// rate limiter is registered in main.go (NOT in this diff). Without the
// fix, control fires false-positive. With the fix:
//
//   - controlVisible=false (no full file loaded) → control SUPPRESSED entirely.
//   - controlVisible=true with the limiter symbol present anywhere in the
//     full file → control NOT raised.
//   - controlVisible=true with the limiter symbol absent from the full file
//     → control raised (the genuine finding).
func TestMatchCandidates_Fixture8_ControlAsymmetry(t *testing.T) {
	smellByTrigger, smellByRole, control := minimalPools()
	addedHandler := `func chatHandler(w http.ResponseWriter, r *http.Request) {
    stream(w, r)
}
`

	t.Run("controlVisible=false suppresses control entirely (the bug we fixed)", func(t *testing.T) {
		cands := matchCandidates("handler", addedHandler, "", false, smellByTrigger, smellByRole, control)
		if hasCandidate(cands, "unauthenticated_endpoints_unlimited", "control") {
			t.Errorf("control raised against diff alone — would block on every routine handler edit. got=%+v", cands)
		}
	})

	t.Run("controlVisible=true with guard PRESENT in full file → no finding", func(t *testing.T) {
		// Simulate: rate limiter registered in main.go which is part of the
		// full file we loaded (in practice we'd load the diff'd file but the
		// limiter could be in main.go too; the same matcher is correct as
		// long as the guard symbol appears anywhere it considers).
		fullFileWithGuard := addedHandler + "\nvar limiter = rate.NewLimiter(1, 1)\n"
		cands := matchCandidates("handler", addedHandler, fullFileWithGuard, true, smellByTrigger, smellByRole, control)
		if hasCandidate(cands, "unauthenticated_endpoints_unlimited", "control") {
			t.Errorf("control raised even though guard rate.NewLimiter is in full file. got=%+v", cands)
		}
	})

	t.Run("controlVisible=true with guard ABSENT in full file → control raised", func(t *testing.T) {
		fullFileNoGuard := addedHandler + "\n// no rate limiter anywhere\n"
		cands := matchCandidates("handler", addedHandler, fullFileNoGuard, true, smellByTrigger, smellByRole, control)
		if !hasCandidate(cands, "unauthenticated_endpoints_unlimited", "control") {
			t.Errorf("control NOT raised even though guard is absent from full file. got=%+v", cands)
		}
	})
}

// -----------------------------------------------------------------
// Sanity check: a clean smell hit fires; affinity is dedup'd against it.
func TestMatchCandidates_SmellAndAffinityDedup(t *testing.T) {
	smellByTrigger, smellByRole, control := minimalPools()
	added := "resp, err := http.DefaultClient.Get(url)\n"

	cands := matchCandidates("handler", added, "", false, smellByTrigger, smellByRole, control)

	smellHit := false
	affinityHit := false
	for _, c := range cands {
		if c.Slug == "metadata_ssrf" && c.Kind == "smell" {
			smellHit = true
		}
		if c.Slug == "metadata_ssrf" && c.Kind == "affinity" {
			affinityHit = true
		}
	}
	if !smellHit {
		t.Errorf("expected metadata_ssrf as smell candidate")
	}
	if affinityHit {
		t.Errorf("metadata_ssrf double-listed as affinity — dedup failed")
	}
}

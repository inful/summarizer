package safety

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecideGuard_ThresholdBoundary: pin the threshold decision rule.
// noul >= threshold = VerdictInjection; noul < threshold = VerdictSafe.
// The HTTP fake returns whatever noul you put in the body.
func TestDecideGuard_ThresholdBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		noul      float64
		threshold float64
		want      Verdict
	}{
		{"below-threshold", 0.50, 0.7, VerdictSafe},
		{"at-threshold", 0.70, 0.7, VerdictInjection}, // >= threshold is injection
		{"above-threshold", 0.95, 0.7, VerdictInjection},
		{"very-low", 0.01, 0.7, VerdictSafe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/v1/systemone") {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"answers":{"prompt_injection":{"type":"noul","noul":%f,"confidence":0.9,"action":{"act_probability":1.0}}}}`, tc.noul)
			}))
			defer srv.Close()

			g := NewDecide(srv.URL, "", tc.threshold)
			v, n, err := g.Check(context.Background(), "test prompt")
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if n != tc.noul {
				t.Errorf("score = %f, want %f", n, tc.noul)
			}
			if v != tc.want {
				t.Errorf("verdict = %q, want %q (noul=%f, threshold=%f)", v, tc.want, tc.noul, tc.threshold)
			}
		})
	}
}

// TestDecideGuard_MalformedResponse: if the API returns a payload without
// the prompt_injection answer, the guard surfaces an error. The pipeline
// policy is hard-fail on any guard error.
func TestDecideGuard_MalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{}}`)) // missing prompt_injection
	}))
	defer srv.Close()

	g := NewDecide(srv.URL, "", 0.7)
	_, _, err := g.Check(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error on malformed response")
	}
}

// TestDecideGuard_RequestShape: the request body sent to /v1/systemone
// carries the prompt as state.body and asks one noul question with our
// specific question text. Structural test — if decide adds a required
// parameter, this fails first.
func TestDecideGuard_RequestShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bs := string(body)
		if !strings.Contains(bs, "test prompt body text") {
			t.Errorf("request body should contain state.body: %s", bs)
		}
		if !strings.Contains(bs, `"prompt_injection"`) {
			t.Errorf("request body should reference prompt_injection question: %s", bs)
		}
		if !strings.Contains(bs, `"type":"noul"`) {
			t.Errorf("request body should declare noul type: %s", bs)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"prompt_injection":{"type":"noul","noul":0.05,"confidence":0.9,"action":{"act_probability":1.0}}}}`))
	}))
	defer srv.Close()

	g := NewDecide(srv.URL, "glpat-test", 0.7)
	if _, _, err := g.Check(context.Background(), "test prompt body text"); err != nil {
		t.Fatalf("check: %v", err)
	}
}

// TestDecideGuard_DefaultThreshold: passing an out-of-range threshold
// falls back to the 0.7 default.
func TestDecideGuard_DefaultThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"prompt_injection":{"type":"noul","noul":0.05,"confidence":0.9,"action":{"act_probability":1.0}}}}`))
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name      string
		threshold float64
	}{
		{"negative", -1},
		{"zero", 0},
		{"over-one", 1.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewDecide(srv.URL, "", tc.threshold)
			v, _, err := g.Check(context.Background(), "test")
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if v != VerdictSafe {
				t.Errorf("default threshold should make 0.05 safe, got %q", v)
			}
		})
	}
}

package bgppeer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDownloadAndVerify_MatchingSHA(t *testing.T) {
	body := "exact bytes we expect\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	// SHA-256 of body computed at test-author time. If body literal
	// changes update this too.
	const wantSHA = "e374d024c2c504e5355786be9e3ec059be693732a2dda332c4c56f935e46d97f"
	got, err := downloadAndVerify(context.Background(), srv.URL, wantSHA)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(got) != body {
		t.Errorf("body mismatch:\ngot:  %q\nwant: %q", got, body)
	}
}

func TestDownloadAndVerify_MismatchSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "tampered content")
	}))
	defer srv.Close()
	const wrongSHA = "0000000000000000000000000000000000000000000000000000000000000000"
	_, err := downloadAndVerify(context.Background(), srv.URL, wrongSHA)
	if err == nil {
		t.Fatal("expected SHA mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "integrity check failed") {
		t.Errorf("expected integrity-check error, got: %v", err)
	}
}

func TestDownloadAndVerify_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	_, err := downloadAndVerify(context.Background(), srv.URL, "ignored")
	if err == nil {
		t.Fatal("expected non-2xx error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("expected HTTP 404 surfaced, got: %v", err)
	}
}

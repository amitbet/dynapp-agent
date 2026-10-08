package shellagent

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeIconDownloadUsesPublishedRevision(t *testing.T) {
	var artwork bytes.Buffer
	if err := png.Encode(&artwork, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("revision") != "rev_icon+new" || r.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("icon request can reuse stale artwork: %s, cache=%q", r.URL, r.Header.Get("Cache-Control"))
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(artwork.Bytes())
	}))
	defer server.Close()
	icon, err := fetchNativeIcon(context.Background(), server.Client(), server.URL, "rev_icon+new")
	if err != nil || !bytes.Equal(icon, artwork.Bytes()) {
		t.Fatalf("downloaded artwork mismatch: %v", err)
	}
}

func TestNativeIconDownloadReportsInvalidPublishedArtwork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer server.Close()
	_, err := fetchNativeIcon(context.Background(), server.Client(), server.URL, "rev_invalid")
	if err == nil || !strings.Contains(err.Error(), "publisher must update") {
		t.Fatalf("invalid artwork should explain how to repair the app: %v", err)
	}
}

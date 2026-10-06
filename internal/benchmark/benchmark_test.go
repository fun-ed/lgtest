package benchmark

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "identity" || !strings.Contains(r.Header.Get("User-Agent"), "Firefox/120.0") {
			t.Error("missing Firefox/identity headers")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1024")
		io.WriteString(w, strings.Repeat("x", 1024))
	}))
	defer server.Close()
	run := Measure(context.Background(), server.Client(), server.URL, "firefox", 1, 0)
	if !run.Success || run.Bytes != 1024 || run.HTTPVersion != "HTTP/1.1" || run.FirstBodySeconds <= 0 {
		t.Fatalf("%+v", run)
	}
}
func TestBadPayloads(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"404", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "missing", 404) }},
		{"HTML", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<html>error</html>")
		}},
		{"truncated", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "2048")
			io.WriteString(w, "short")
		}},
		{"empty", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Length", "0") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(tc.handler)
			defer s.Close()
			run := Measure(context.Background(), s.Client(), s.URL, "go", 1, 0)
			if run.Success || run.Error == "" {
				t.Fatalf("%+v", run)
			}
		})
	}
}
func TestByteCapAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		io.WriteString(w, strings.Repeat("x", 1024))
	}))
	defer server.Close()
	run := Measure(context.Background(), server.Client(), server.URL, "go", 1, 128)
	if !run.Success || !run.Limited || run.Bytes != 128 {
		t.Fatalf("%+v", run)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run = Measure(ctx, &http.Client{Timeout: time.Second}, server.URL, "go", 1, 0)
	if run.Success {
		t.Fatal("cancelled download succeeded")
	}
}
func TestSummary(t *testing.T) {
	s := Summarize([]Run{{Success: true, Bytes: 1000000, TotalSeconds: 1, Mbps: 8}, {Success: true, Bytes: 1000000, TotalSeconds: 2, Mbps: 4}, {Success: false, Bytes: 5}})
	if s.Successful != 2 || s.Failed != 1 || s.TotalBytes != 2000005 || s.MeanMbps != 6 || s.MedianMbps != 6 || s.AggregateMbps != 16.0/3 {
		t.Fatalf("%+v", s)
	}
}

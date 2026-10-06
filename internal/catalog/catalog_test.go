package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPsychzParse(t *testing.T) {
	fixture := `<a href="https://lg.jp.psychz.net/"></a><a href="https://lg.jp.psychz.net/">Tokyo, Japan</a><a href="https://evil.example/">Ignore</a><ul><li>Server Location: <b>Taipei, Taiwan</b></li><li>Test IPv4: 103.59.110.170</li><li>Test IPv6: 2604:6600:4b:2::3</li><li>Test files: <a href="200MB.test">200MB</a></li><li>Your IP Address: 192.0.2.1</li></ul>`
	current, regions, err := Parse("psychz", Seeds["psychz"], strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != "psychz:tw" || current.Name != "Taipei, Taiwan" || current.IPv4 != "103.59.110.170" || current.IPv6 != "2604:6600:4b:2::3" {
		t.Fatalf("unexpected public metadata: %+v", current)
	}
	if len(current.Files) != 1 || current.Files[0].URL != "https://lg.tw.psychz.net/200MB.test" {
		t.Fatalf("files %+v", current.Files)
	}
	if len(regions) != 2 {
		t.Fatalf("regions %+v", regions)
	}
	for _, r := range regions {
		if r.ID == "psychz:jp" && r.Name != "Tokyo, Japan" {
			t.Fatal("lost location label")
		}
	}
}
func TestVultrParse(t *testing.T) {
	fixture := `<select><option data-url="https://hnd-jp-ping.vultr.com/" selected="selected">Tokyo</option><option data-url="https://ams-nl-ping.vultr.com/">Amsterdam</option></select><li>Test IPv4: <a>108.61.201.151</a></li><li>Test IPv6: <a>2001:19f0:7000:8002:5400:ff:fe00:536d</a></li><li>Test files: <a href="/vultr.com.100MB.bin">100MB</a><a href="/vultr.com.1000MB.bin">1GB</a></li>`
	current, found, err := Parse("vultr", Seeds["vultr"], strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if current.Name != "Tokyo" || len(found) != 2 || len(current.Files) != 2 || current.Files[0].Label != "100MB" {
		t.Fatalf("unexpected %+v %+v", current, found)
	}
}
func TestRejectForeignFileAndMissingData(t *testing.T) {
	_, _, err := Parse("vultr", Seeds["vultr"], strings.NewReader(`<li>Test files: <a href="https://outside.example/test.bin">100MB</a></li>`))
	if err == nil {
		t.Fatal("accepted foreign file")
	}
	_, _, err = Parse("psychz", "http://lg.tw.psychz.net/", strings.NewReader(""))
	if err == nil {
		t.Fatal("accepted insecure seed")
	}
}
func TestSaveNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := SaveNew(path, map[string]int{"ok": 1}); err != nil {
		t.Fatal(err)
	}
	if err := SaveNew(path, map[string]int{"ok": 2}); err == nil {
		t.Fatal("overwrote existing result")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"ok": 1`) {
		t.Fatal("original changed")
	}
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fun-ed/lgtest/internal/benchmark"
	"github.com/fun-ed/lgtest/internal/catalog"
	"github.com/fun-ed/lgtest/internal/netclient"
)

type report struct {
	SchemaVersion    int               `json:"schema_version"`
	StartedAt        time.Time         `json:"started_at"`
	CatalogFetchedAt time.Time         `json:"catalog_fetched_at"`
	Region           catalog.Region    `json:"region"`
	File             catalog.File      `json:"file"`
	Mode             string            `json:"mode"`
	Proxy            string            `json:"proxy,omitempty"`
	Family           string            `json:"destination_family"`
	ConnectIP        string            `json:"connect_ip,omitempty"`
	Profile          string            `json:"profile"`
	FingerprintScope string            `json:"fingerprint_scope"`
	MaxBytes         int64             `json:"max_bytes,omitempty"`
	Runs             []benchmark.Run   `json:"runs"`
	Summary          benchmark.Summary `json:"summary"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
func execute(ctx context.Context, args []string, input io.Reader, output, diagnostic io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(output, "lgtest refresh|list|run\n  refresh --output catalog/new.json [--provider all|psychz|vultr]\n  list [--catalog PATH] [--provider all|psychz|vultr]\n  run [--region ID --file LABEL] [--mode direct|http|socks5] [--proxy URL]\n      [--family auto|4|6] [--profile firefox|go] [--runs 1] [--output NEW.json]\n      [--max-bytes N] [--timeout 180s] [--interval 2s] [--interactive]\nFirefox profile = Firefox 120 headers + uTLS ClientHello, not full current browser behavior.")
		return nil
	}
	command := args[0]
	if command != "refresh" && command != "list" && command != "run" {
		return errors.New("unknown command; use lgtest help")
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(diagnostic)
	catalogPath := fs.String("catalog", "", "JSON catalog; default bundled Camofox snapshot")
	provider := fs.String("provider", "all", "all, psychz or vultr")
	regionID := fs.String("region", "", "exact region ID from list")
	fileLabel := fs.String("file", "", "advertised file label, e.g. 100MB")
	mode := fs.String("mode", "", "direct, http or socks5")
	proxyURL := fs.String("proxy", "", "http:// or socks5[h]:// URL, no credentials")
	family := fs.String("family", "auto", "destination auto, 4 or 6")
	profile := fs.String("profile", "firefox", "firefox (Firefox 120 preset) or go")
	runs := fs.Int("runs", 1, "sequential downloads")
	interval := fs.Duration("interval", 2*time.Second, "pause between downloads")
	timeout := fs.Duration("timeout", 180*time.Second, "per download deadline")
	maxBytes := fs.Int64("max-bytes", 0, "optional partial-download cap; 0 means full file")
	resultPath := fs.String("output", "", "new JSON output; never overwrites")
	interactive := fs.Bool("interactive", false, "prompt using stdin, including piped input")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *provider != "all" && *provider != "psychz" && *provider != "vultr" {
		return errors.New("invalid provider")
	}
	if *family != "auto" && *family != "4" && *family != "6" {
		return errors.New("invalid destination family")
	}
	if *profile != "firefox" && *profile != "go" {
		return errors.New("invalid profile")
	}
	if *runs < 1 || *runs > 100 || *interval < 0 || *timeout <= 0 || *maxBytes < 0 {
		return errors.New("invalid runs, interval, timeout or max-bytes")
	}
	if command == "refresh" {
		if *resultPath == "" {
			return errors.New("refresh requires --output NEW.json")
		}
		if _, err := os.Stat(*resultPath); err == nil {
			return errors.New("output already exists")
		} else if !os.IsNotExist(err) {
			return err
		}
		client, err := netclient.New(netclient.Options{ProxyURL: *proxyURL, Family: "auto", Profile: *profile, Timeout: 20 * time.Second})
		if err != nil {
			return err
		}
		c, err := catalog.Refresh(ctx, client, *provider)
		if err != nil {
			return err
		}
		if err := catalog.SaveNew(*resultPath, c); err != nil {
			return err
		}
		failed := 0
		for _, r := range c.Regions {
			if r.Error != "" {
				failed++
			}
		}
		fmt.Fprintf(output, "Saved %d regions; %d page failures: %s\n", len(c.Regions), failed, *resultPath)
		if failed > 0 {
			return errors.New("partial catalog saved; failed pages are marked")
		}
		return nil
	}
	var c catalog.Catalog
	var err error
	if *catalogPath == "" {
		c, err = catalog.Bundled()
	} else {
		c, err = catalog.Load(*catalogPath)
	}
	if err != nil {
		return err
	}
	regions := []catalog.Region{}
	for _, r := range c.Regions {
		if *provider == "all" || r.Provider == *provider {
			regions = append(regions, r)
		}
	}
	if command == "list" {
		fmt.Fprintf(output, "Catalog: %s (%s)\n", c.FetchedAt.Format(time.RFC3339), c.Method)
		for _, r := range regions {
			labels := []string{}
			for _, f := range r.Files {
				labels = append(labels, f.Label)
			}
			fmt.Fprintf(output, "%s | %s | IPv4 %s | IPv6 %s | %s", r.ID, r.Name, r.IPv4, r.IPv6, strings.Join(labels, ", "))
			if r.Error != "" {
				fmt.Fprintf(output, " | ERROR: %s", r.Error)
			}
			fmt.Fprintln(output)
		}
		return nil
	}
	if len(regions) == 0 {
		return errors.New("no regions for provider")
	}
	terminal := false
	if f, ok := input.(*os.File); ok {
		if info, e := f.Stat(); e == nil {
			terminal = info.Mode()&os.ModeCharDevice != 0
		}
	}
	prompt := *interactive || terminal
	reader := bufio.NewReader(input)
	choose := func(title string, options []string) (int, error) {
		fmt.Fprintln(diagnostic, title)
		for i, value := range options {
			fmt.Fprintf(diagnostic, "%d. %s\n", i+1, value)
		}
		fmt.Fprint(diagnostic, "> ")
		line, err := reader.ReadString('\n')
		if err != nil && !(err == io.EOF && len(line) > 0) {
			return 0, errors.New("no selection input")
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > len(options) {
			return 0, errors.New("selection out of range")
		}
		return n - 1, nil
	}
	selected := -1
	if *regionID != "" {
		for i, r := range regions {
			if r.ID == *regionID {
				selected = i
			}
		}
		if selected < 0 {
			return errors.New("unknown region ID")
		}
	} else {
		if !prompt {
			return errors.New("non-interactive run requires --region")
		}
		options := []string{}
		for _, r := range regions {
			options = append(options, r.ID+" · "+r.Name)
		}
		selected, err = choose("Select region", options)
		if err != nil {
			return err
		}
	}
	region := regions[selected]
	if region.Error != "" || len(region.Files) == 0 {
		return errors.New("region has no verified file links; refresh catalog")
	}
	fileIndex := -1
	if *fileLabel != "" {
		for i, f := range region.Files {
			if strings.EqualFold(f.Label, *fileLabel) {
				fileIndex = i
			}
		}
		if fileIndex < 0 {
			return errors.New("file label not advertised in region")
		}
	} else if len(region.Files) == 1 {
		fileIndex = 0
	} else {
		if !prompt {
			return errors.New("region has multiple files; specify --file")
		}
		options := []string{}
		for _, f := range region.Files {
			options = append(options, f.Label+" · "+f.URL)
		}
		fileIndex, err = choose("Select test file", options)
		if err != nil {
			return err
		}
	}
	if *mode == "" {
		if prompt {
			i, e := choose("Select route", []string{"direct", "http proxy", "socks5 proxy"})
			if e != nil {
				return e
			}
			*mode = []string{"direct", "http", "socks5"}[i]
		} else {
			*mode = "direct"
		}
	}
	if *mode != "direct" && *mode != "http" && *mode != "socks5" {
		return errors.New("invalid mode")
	}
	if *mode == "direct" && *proxyURL != "" {
		return errors.New("direct mode cannot accept --proxy")
	}
	if *mode != "direct" && *proxyURL == "" {
		if *mode == "http" {
			*proxyURL = "http://127.0.0.1:8080"
		} else {
			*proxyURL = "socks5h://127.0.0.1:1080"
		}
	}
	if *proxyURL != "" {
		p, e := url.Parse(*proxyURL)
		if e != nil || p.User != nil {
			return errors.New("invalid proxy; credentials are not accepted")
		}
		if (*mode == "http" && p.Scheme != "http") || (*mode == "socks5" && p.Scheme != "socks5" && p.Scheme != "socks5h") {
			return errors.New("proxy scheme does not match mode")
		}
	}
	connectIP := ""
	if *family == "4" {
		connectIP = region.IPv4
	}
	if *family == "6" {
		connectIP = region.IPv6
	}
	if *family != "auto" && connectIP == "" {
		return errors.New("requested family has no advertised test IP")
	}
	file := region.Files[fileIndex]
	target, e := url.Parse(file.URL)
	if e != nil {
		return e
	}
	connectHost := ""
	if connectIP != "" {
		connectHost = target.Hostname()
	}
	client, e := netclient.New(netclient.Options{ProxyURL: *proxyURL, Family: *family, Profile: *profile, ConnectHost: connectHost, ConnectIP: connectIP, Timeout: *timeout})
	if e != nil {
		return e
	}
	data := report{SchemaVersion: 1, StartedAt: time.Now().UTC(), CatalogFetchedAt: c.FetchedAt, Region: region, File: file, Mode: *mode, Proxy: *proxyURL, Family: *family, ConnectIP: connectIP, Profile: *profile, MaxBytes: *maxBytes, Runs: []benchmark.Run{}, FingerprintScope: "Firefox 120 headers/uTLS ClientHello only; HTTP2/header order not full Firefox; GOST upstream unchanged"}
	if *profile == "go" {
		data.FingerprintScope = "Go native transport; GOST upstream unchanged"
	}
	var resultFile *os.File
	if *resultPath != "" {
		resultFile, e = os.OpenFile(*resultPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		defer resultFile.Close()
	}
	persist := func() error {
		data.Summary = benchmark.Summarize(data.Runs)
		if resultFile == nil {
			return nil
		}
		if _, e := resultFile.Seek(0, 0); e != nil {
			return e
		}
		encoder := json.NewEncoder(resultFile)
		encoder.SetIndent("", "  ")
		if e := encoder.Encode(data); e != nil {
			return e
		}
		offset, e := resultFile.Seek(0, io.SeekCurrent)
		if e != nil {
			return e
		}
		return resultFile.Truncate(offset)
	}
	if e := persist(); e != nil {
		return e
	}
	fmt.Fprintf(diagnostic, "%s %s · %s · family %s · %d run(s) · body discarded\n", region.ID, file.Label, *mode, *family, *runs)
	if *maxBytes > 0 {
		fmt.Fprintln(diagnostic, "PARTIAL DOWNLOAD: byte cap enabled; not a full-file benchmark")
	}
	for n := 1; n <= *runs; n++ {
		r := benchmark.Measure(ctx, client, file.URL, *profile, n, *maxBytes)
		data.Runs = append(data.Runs, r)
		if e := persist(); e != nil {
			return e
		}
		if e := json.NewEncoder(output).Encode(r); e != nil {
			return e
		}
		if ctx.Err() != nil {
			break
		}
		if n < *runs {
			timer := time.NewTimer(*interval)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
			if ctx.Err() != nil {
				break
			}
		}
	}
	data.Summary = benchmark.Summarize(data.Runs)
	if e := json.NewEncoder(output).Encode(data.Summary); e != nil {
		return e
	}
	if data.Summary.Failed > 0 || ctx.Err() != nil {
		return errors.New("one or more downloads failed/cancelled; inspect saved measurements")
	}
	return nil
}

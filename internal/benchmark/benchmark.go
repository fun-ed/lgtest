package benchmark

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Run struct {
	Number           int     `json:"run"`
	Success          bool    `json:"success"`
	Status           int     `json:"http_status"`
	HTTPVersion      string  `json:"target_http_version,omitempty"`
	Bytes            int64   `json:"bytes"`
	ContentLength    int64   `json:"content_length"`
	HeaderSeconds    float64 `json:"response_header_seconds"`
	FirstBodySeconds float64 `json:"first_body_byte_seconds"`
	TotalSeconds     float64 `json:"total_seconds"`
	TransferSeconds  float64 `json:"transfer_seconds"`
	Mbps             float64 `json:"average_mbps"`
	TransferMbps     float64 `json:"transfer_mbps"`
	Limited          bool    `json:"limited"`
	Error            string  `json:"error,omitempty"`
}
type Summary struct {
	Successful    int     `json:"successful_runs"`
	Failed        int     `json:"failed_runs"`
	TotalBytes    int64   `json:"bytes_all_runs"`
	MeanMbps      float64 `json:"mean_mbps"`
	MedianMbps    float64 `json:"median_mbps"`
	AggregateMbps float64 `json:"aggregate_mbps"`
	MinMbps       float64 `json:"min_mbps"`
	MaxMbps       float64 `json:"max_mbps"`
}

func Measure(ctx context.Context, client *http.Client, url, profile string, number int, maxBytes int64) Run {
	r := Run{Number: number, ContentLength: -1}
	start := time.Now()
	finish := func() {
		r.TotalSeconds = time.Since(start).Seconds()
		if r.TotalSeconds > 0 {
			r.Mbps = float64(r.Bytes) * 8 / 1e6 / r.TotalSeconds
		}
		if r.TransferSeconds > 0 {
			r.TransferMbps = float64(r.Bytes) * 8 / 1e6 / r.TransferSeconds
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		r.Error = "invalid request"
		finish()
		return r
	}
	if profile == "firefox" {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:120.0) Gecko/20100101 Firefox/120.0")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Language", "zh-TW,zh;q=0.9,en-US;q=0.8,en;q=0.7")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Site", "none")
	} else {
		req.Header.Set("User-Agent", "gostlab-lgtest/1")
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	r.HeaderSeconds = time.Since(start).Seconds()
	if err != nil {
		if ctx.Err() != nil {
			r.Error = ctx.Err().Error()
		} else {
			r.Error = "request/transport failed"
		}
		finish()
		return r
	}
	defer resp.Body.Close()
	r.Status, r.HTTPVersion, r.ContentLength = resp.StatusCode, resp.Proto, resp.ContentLength
	if resp.StatusCode != 200 {
		r.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		finish()
		return r
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		r.Error = "HTML response is not a test file"
		finish()
		return r
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		r.Error = "unexpected encoded payload"
		finish()
		return r
	}
	// Read an actual payload byte; receiving headers is not the same as first body byte.
	transferStart := time.Now()
	first := make([]byte, 1)
	n, readErr := io.ReadFull(resp.Body, first)
	r.Bytes = int64(n)
	if n > 0 {
		r.FirstBodySeconds = time.Since(start).Seconds()
	}
	if readErr != nil && readErr != io.EOF {
		r.Error = "failed before first body byte"
	} else if readErr == nil {
		var reader io.Reader = resp.Body
		if maxBytes > 0 {
			reader = io.LimitReader(resp.Body, maxBytes-r.Bytes)
		}
		copied, copyErr := io.Copy(io.Discard, reader)
		r.Bytes += copied
		if copyErr != nil {
			r.Error = "body read failed or timed out"
		}
	}
	r.TransferSeconds = time.Since(transferStart).Seconds()
	r.Limited = maxBytes > 0 && r.Bytes >= maxBytes && (r.ContentLength < 0 || r.ContentLength > r.Bytes)
	if r.Error == "" && !r.Limited && r.ContentLength >= 0 && r.Bytes != r.ContentLength {
		r.Error = "incomplete Content-Length"
	}
	if r.Error == "" && r.Bytes == 0 {
		r.Error = "empty test file"
	}
	r.Success = r.Error == ""
	finish()
	return r
}

func Summarize(runs []Run) Summary {
	s := Summary{}
	speeds := []float64{}
	totalSeconds := 0.0
	goodBytes := int64(0)
	for _, r := range runs {
		s.TotalBytes += r.Bytes
		if !r.Success {
			s.Failed++
			continue
		}
		s.Successful++
		speeds = append(speeds, r.Mbps)
		goodBytes += r.Bytes
		totalSeconds += r.TotalSeconds
	}
	if len(speeds) == 0 {
		return s
	}
	sort.Float64s(speeds)
	for _, speed := range speeds {
		s.MeanMbps += speed
	}
	s.MeanMbps /= float64(len(speeds))
	middle := len(speeds) / 2
	s.MedianMbps = speeds[middle]
	if len(speeds)%2 == 0 {
		s.MedianMbps = (speeds[middle-1] + speeds[middle]) / 2
	}
	s.MinMbps, s.MaxMbps = speeds[0], speeds[len(speeds)-1]
	if totalSeconds > 0 {
		s.AggregateMbps = float64(goodBytes) * 8 / 1e6 / totalSeconds
	}
	if math.IsNaN(s.AggregateMbps) {
		s.AggregateMbps = 0
	}
	return s
}

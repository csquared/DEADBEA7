package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/csquared/deadalyze/client"
)

func collect(t *testing.T, files []string, opts *analyzeOptions, events ...client.Event) (*bytes.Buffer, *collector) {
	t.Helper()
	var out bytes.Buffer
	c := newCollector(files, opts, &out, io.Discard)
	for _, ev := range events {
		c.on(ev)
	}
	c.finish(nil)
	return &out, c
}

func lines(t *testing.T, out *bytes.Buffer) []map[string]any {
	t.Helper()
	var got []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("bad output line %q: %v", line, err)
		}
		got = append(got, obj)
	}
	return got
}

func TestCollectorInputOrder(t *testing.T) {
	files := []string{"first.mp3", "second.mp3"}
	out, _ := collect(t, files, &analyzeOptions{},
		client.Event{Event: client.ItemDone, ID: "1", Tasks: map[string]string{"grid": "ok"}},
		client.Event{Event: client.ItemDone, ID: "0", Tasks: map[string]string{"grid": "ok"}},
	)
	got := lines(t, out)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(got), out.String())
	}
	if got[0]["file"] != "first.mp3" || got[1]["file"] != "second.mp3" {
		t.Fatalf("out of order: %v", got)
	}
	if got[0]["ok"] != true || got[1]["ok"] != true {
		t.Fatalf("ok fields wrong: %v", got)
	}
}

func TestCollectorWaveformFilter(t *testing.T) {
	files := []string{"track.m4a"}
	blob := client.Inline([]byte{1, 2, 3})
	events := []client.Event{
		{Event: client.ItemStarted, ID: "0", DurationMs: 307000},
		{Event: client.WaveformEvent, ID: "0", Kind: "mono_preview", EntryBytes: 1, EntryCount: 2, Rate: 3, Data: &blob},
		{Event: client.WaveformEvent, ID: "0", Kind: "three_band_preview", EntryBytes: 1, EntryCount: 2, Rate: 3, Data: &blob},
		{Event: client.ItemDone, ID: "0", Tasks: map[string]string{"waveform": "ok"}},
	}

	t.Run("none", func(t *testing.T) {
		out, _ := collect(t, files, &analyzeOptions{}, events...)
		got := lines(t, out)
		if _, ok := got[0]["waveforms"]; ok {
			t.Fatalf("waveforms present with no filter: %v", got[0])
		}
	})

	t.Run("one kind", func(t *testing.T) {
		out, _ := collect(t, files, &analyzeOptions{wavKinds: map[string]bool{"three_band_preview": true}}, events...)
		got := lines(t, out)
		waves, ok := got[0]["waveforms"].(map[string]any)
		if !ok {
			t.Fatalf("waveforms missing: %v", got[0])
		}
		if len(waves) != 1 {
			t.Fatalf("got %d waveforms, want 1: %v", len(waves), waves)
		}
		gotWave := waves["three_band_preview"].(map[string]any)
		if gotWave["entry_bytes"].(float64) != 1 || gotWave["entry_count"].(float64) != 2 || gotWave["rate"].(float64) != 3 {
			t.Fatalf("waveform metadata wrong: %v", gotWave)
		}
		if gotWave["b64"] != base64.StdEncoding.EncodeToString([]byte{1, 2, 3}) {
			t.Fatalf("waveform b64 wrong: %v", gotWave["b64"])
		}
	})

	t.Run("all", func(t *testing.T) {
		out, _ := collect(t, files, &analyzeOptions{wavAll: true}, events...)
		got := lines(t, out)
		waves := got[0]["waveforms"].(map[string]any)
		if len(waves) != 2 {
			t.Fatalf("got %d waveforms, want 2: %v", len(waves), waves)
		}
		for _, kind := range []string{"mono_preview", "three_band_preview"} {
			if _, ok := waves[kind]; !ok {
				t.Fatalf("missing waveform %s: %v", kind, waves)
			}
		}
	})
}

func TestCollectorItemError(t *testing.T) {
	out, c := collect(t, []string{"bad.mp3"}, &analyzeOptions{},
		client.Event{Event: client.ItemError, ID: "0", Task: client.TaskGrid, Code: "grid_failed", Message: "boom"},
	)
	got := lines(t, out)
	if len(got) != 1 || got[0]["ok"] != false {
		t.Fatalf("ok not false: %v", got)
	}
	errs, ok := got[0]["errors"].([]any)
	if !ok || len(errs) != 1 {
		t.Fatalf("errors wrong: %v", got[0]["errors"])
	}
	first := errs[0].(map[string]any)
	if first["task"] != "grid" || first["code"] != "grid_failed" || first["message"] != "boom" {
		t.Fatalf("error wrong: %v", first)
	}
	if c.exitCode() != 1 {
		t.Fatalf("exit code = %d, want 1", c.exitCode())
	}
}

func TestCollectorBatchFailure(t *testing.T) {
	files := []string{"first.mp3", "second.mp3"}
	var out bytes.Buffer
	c := newCollector(files, &analyzeOptions{}, &out, io.Discard)
	c.on(client.Event{Event: client.ItemDone, ID: "0", Tasks: map[string]string{"grid": "ok"}})
	c.on(client.Event{Event: client.BatchError, Code: "invalid_request", Message: "nope"})
	c.finish(nil)

	got := lines(t, &out)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2: %v", len(got), out.String())
	}
	if got[0]["file"] != "first.mp3" || got[0]["ok"] != true {
		t.Fatalf("first file wrong: %v", got[0])
	}
	if got[1]["file"] != "second.mp3" || got[1]["ok"] != false {
		t.Fatalf("second file wrong: %v", got[1])
	}
	errs := got[1]["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("errors wrong: %v", got[1]["errors"])
	}
	first := errs[0].(map[string]any)
	if first["task"] != "batch" || first["code"] != "invalid_request" || first["message"] != "nope" {
		t.Fatalf("batch error wrong: %v", first)
	}
	if c.exitCode() != 1 {
		t.Fatalf("exit code = %d, want 1", c.exitCode())
	}
}

func TestParseAnalyzeFlags(t *testing.T) {
	opts, files, err := parseAnalyzeFlags([]string{
		"-tasks", "grid,key",
		"-waveforms", "three_band_preview, color_detail",
		"-beats",
		"-settings", "dance4x4",
		"a.mp3", "b.m4a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opts.tasks, []string{"grid", "key"}) {
		t.Fatalf("tasks = %v", opts.tasks)
	}
	if opts.wavAll || len(opts.wavKinds) != 2 || !opts.wavKinds["three_band_preview"] || !opts.wavKinds["color_detail"] {
		t.Fatalf("waveforms = %v %v", opts.wavAll, opts.wavKinds)
	}
	if !opts.beats || opts.settings != "dance4x4" {
		t.Fatalf("beats/settings = %v %q", opts.beats, opts.settings)
	}
	if !reflect.DeepEqual(files, []string{"a.mp3", "b.m4a"}) {
		t.Fatalf("files = %v", files)
	}

	opts, files, err = parseAnalyzeFlags([]string{"a.mp3"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opts.tasks, []string{"grid", "key", "waveform"}) {
		t.Fatalf("default tasks = %v", opts.tasks)
	}
	if opts.wavAll || opts.wavKinds != nil || opts.beats || opts.settings != "dance4x4" {
		t.Fatalf("defaults wrong: %+v", opts)
	}
	if !reflect.DeepEqual(files, []string{"a.mp3"}) {
		t.Fatalf("files = %v", files)
	}

	opts, _, err = parseAnalyzeFlags([]string{"-waveforms", "all", "a.mp3"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.wavAll {
		t.Fatalf("-waveforms all not parsed: %+v", opts)
	}
}

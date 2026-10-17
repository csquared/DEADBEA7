// Command deadbea7 is a small command line over the analysis engine's Go
// client: it describes the installed bundle and analyses audio files to
// JSON Lines.
//
//	deadbea7 describe
//	deadbea7 analyze [-tasks grid,key,waveform] [-waveforms KIND,...|all] [-beats] [-settings dance4x4] FILE...
//
// describe prints the engine's describe document as indented JSON. analyze
// runs one batch over all files and prints one JSON object per file, in
// input order, with the grid, key, the selected waveform blobs, and any
// errors. It exits 0 when every file is ok, 1 when any file failed, and 2
// on usage errors or when no bundle can be resolved.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/csquared/deadalyze/client"
)

const usage = `usage: deadbea7 describe
       deadbea7 analyze [-tasks grid,key,waveform] [-waveforms KIND,...|all] [-beats] [-settings dance4x4] FILE...`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run carries out one command and returns its exit status: 0 when every
// analysed file is ok, 1 when any failed, and 2 for usage errors or when
// the bundle cannot be resolved.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "describe":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: deadbea7 describe")
			return 2
		}
		eng, err := client.Resolve()
		if err != nil {
			fmt.Fprintf(stderr, "deadbea7: %v\n", err)
			return 2
		}
		d, err := eng.Describe(context.Background())
		if err != nil {
			fmt.Fprintf(stderr, "deadbea7: %v\n", err)
			return 2
		}
		b, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "deadbea7: %v\n", err)
			return 2
		}
		fmt.Fprintln(stdout, string(b))
		return 0
	case "analyze":
		return runAnalyze(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "deadbea7: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

func runAnalyze(args []string, stdout, stderr io.Writer) int {
	opts, files, err := parseAnalyzeFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "deadbea7 analyze: %v\n%s\n", err, usage)
		return 2
	}
	if len(files) == 0 {
		fmt.Fprintf(stderr, "deadbea7 analyze: no input files\n%s\n", usage)
		return 2
	}
	eng, err := client.Resolve()
	if err != nil {
		fmt.Fprintf(stderr, "deadbea7: %v\n", err)
		return 2
	}
	eng.Stderr = stderr

	items := make([]client.Item, len(files))
	for i, f := range files {
		abs, err := filepath.Abs(f)
		if err != nil {
			fmt.Fprintf(stderr, "deadbea7: %v\n", err)
			return 2
		}
		items[i] = client.Item{ID: strconv.Itoa(i), Audio: abs, Tasks: opts.tasks}
	}

	c := newCollector(files, opts, stdout, stderr)
	req := client.Request{
		Settings: map[string]any{"name": opts.settings},
		Items:    items,
	}
	c.finish(eng.AnalyzeAll(context.Background(), req, 0, c.on))
	return c.exitCode()
}

type analyzeOptions struct {
	tasks    []string
	wavAll   bool
	wavKinds map[string]bool
	beats    bool
	settings string
}

// parseAnalyzeFlags parses the analyze subcommand's flags and files.
func parseAnalyzeFlags(args []string) (*analyzeOptions, []string, error) {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tasksCSV := fs.String("tasks", "grid,key,waveform", "comma-separated tasks (grid,key,features,waveform,cues)")
	waveformsCSV := fs.String("waveforms", "", "comma-separated waveform kinds to print, or all (default: none)")
	beats := fs.Bool("beats", false, "include the beat list in each result")
	settings := fs.String("settings", "dance4x4", "settings document name")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}

	tasks, err := parseCSV(*tasksCSV)
	if err != nil {
		return nil, nil, fmt.Errorf("-tasks: %w", err)
	}
	for _, t := range tasks {
		if !validTask(t) {
			return nil, nil, fmt.Errorf("-tasks: unknown task %q", t)
		}
	}

	opts := &analyzeOptions{tasks: tasks, beats: *beats, settings: *settings}
	if *waveformsCSV == "all" {
		opts.wavAll = true
	} else if *waveformsCSV != "" {
		kinds, err := parseCSV(*waveformsCSV)
		if err != nil {
			return nil, nil, fmt.Errorf("-waveforms: %w", err)
		}
		opts.wavKinds = make(map[string]bool, len(kinds))
		for _, k := range kinds {
			opts.wavKinds[k] = true
		}
	}
	return opts, fs.Args(), nil
}

func validTask(t string) bool {
	switch t {
	case client.TaskGrid, client.TaskKey, client.TaskFeatures, client.TaskWaveform, client.TaskCues:
		return true
	}
	return false
}

func parseCSV(s string) ([]string, error) {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("empty item in %q", s)
		}
		out = append(out, p)
	}
	return out, nil
}

// keyResult is the key event trimmed to what the command line prints.
type keyResult struct {
	Camelot    string  `json:"camelot"`
	Label      string  `json:"label"`
	Confidence float64 `json:"confidence"`
}

// waveformResult is one selected waveform blob, base64 of its bytes.
type waveformResult struct {
	EntryBytes int    `json:"entry_bytes"`
	EntryCount int    `json:"entry_count"`
	Rate       int    `json:"rate"`
	B64        string `json:"b64"`
}

// itemError is one failed task, or the batch failure in finish.
type itemError struct {
	Task    string `json:"task"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// result is the JSON object printed for one file (Contract A).
type result struct {
	File            string                    `json:"file"`
	OK              bool                      `json:"ok"`
	DurationMs      *uint32                   `json:"duration_ms,omitempty"`
	BPM             *float64                  `json:"bpm,omitempty"`
	BeatsPerBar     *int                      `json:"beats_per_bar,omitempty"`
	FirstDownbeatMs *int64                    `json:"first_downbeat_ms,omitempty"`
	Key             *keyResult                `json:"key,omitempty"`
	Waveforms       map[string]waveformResult `json:"waveforms,omitempty"`
	Beats           [][2]int64                `json:"beats,omitempty"`
	Errors          []itemError               `json:"errors"`
}

type batchFailure struct {
	code    string
	message string
}

// collector aggregates engine events per file and writes the JSON Lines
// results in input order, as soon as the contiguous prefix of files is
// complete.
type collector struct {
	files    []string
	items    []*result
	ready    []bool
	printed  []bool
	next     int
	out      io.Writer
	errw     io.Writer
	wavAll   bool
	wavKinds map[string]bool
	beats    bool
	batchErr *batchFailure
}

func newCollector(files []string, opts *analyzeOptions, out, errw io.Writer) *collector {
	c := &collector{
		files:    files,
		items:    make([]*result, len(files)),
		ready:    make([]bool, len(files)),
		printed:  make([]bool, len(files)),
		out:      out,
		errw:     errw,
		wavAll:   opts.wavAll,
		wavKinds: opts.wavKinds,
		beats:    opts.beats,
	}
	for i, f := range files {
		c.items[i] = &result{File: f, OK: true, Errors: []itemError{}}
	}
	return c
}

func (c *collector) index(id string) (int, bool) {
	n, err := strconv.Atoi(id)
	if err != nil || n < 0 || n >= len(c.items) {
		return 0, false
	}
	return n, true
}

func (c *collector) wantWaveform(kind string) bool {
	if c.wavAll {
		return true
	}
	return c.wavKinds != nil && c.wavKinds[kind]
}

func (c *collector) on(ev client.Event) {
	switch ev.Event {
	case client.BatchError:
		c.batchErr = &batchFailure{code: ev.Code, message: ev.Message}
		fmt.Fprintf(c.errw, "batch error: %s: %s\n", ev.Code, ev.Message)
		return
	case client.Progress:
		fmt.Fprintf(c.errw, "%s %d/%d\n", ev.Stage, ev.Done, ev.Total)
		return
	case client.ItemWarning:
		fmt.Fprintf(c.errw, "%s: warning %s (%s): %s\n", ev.ID, ev.Task, ev.Code, ev.Message)
		return
	}

	i, ok := c.index(ev.ID)
	if !ok {
		return
	}
	r := c.items[i]
	switch ev.Event {
	case client.ItemStarted:
		if ev.DurationMs != 0 {
			d := ev.DurationMs
			r.DurationMs = &d
		}
	case client.WaveformEvent:
		if ev.DurationMs != 0 && r.DurationMs == nil {
			d := ev.DurationMs
			r.DurationMs = &d
		}
		if !c.wantWaveform(ev.Kind) || ev.Data == nil {
			return
		}
		b, err := ev.Data.Bytes()
		if err != nil {
			return
		}
		if r.Waveforms == nil {
			r.Waveforms = map[string]waveformResult{}
		}
		r.Waveforms[ev.Kind] = waveformResult{
			EntryBytes: ev.EntryBytes,
			EntryCount: ev.EntryCount,
			Rate:       ev.Rate,
			B64:        base64.StdEncoding.EncodeToString(b),
		}
	case client.GridEvent:
		if ev.BPM != 0 {
			bpm := ev.BPM
			r.BPM = &bpm
		}
		if ev.BeatsPerBar != 0 {
			bpb := ev.BeatsPerBar
			r.BeatsPerBar = &bpb
		}
		r.FirstDownbeatMs = ev.FirstDownbeatMs
		if c.beats {
			r.Beats = ev.Beats
		}
	case client.KeyEvent:
		r.Key = &keyResult{Camelot: ev.Camelot, Label: ev.Label, Confidence: ev.Confidence}
	case client.ItemError:
		c.addError(i, ev.Task, ev.Code, ev.Message)
	case client.ItemDone:
		for task, status := range ev.Tasks {
			if status == "failed" {
				c.addError(i, task, "failed", "")
			}
		}
		c.ready[i] = true
		c.flush()
	}
}

// addError marks the file failed and records the error once per task.
func (c *collector) addError(i int, task, code, message string) {
	r := c.items[i]
	r.OK = false
	for _, e := range r.Errors {
		if e.Task == task {
			return
		}
	}
	r.Errors = append(r.Errors, itemError{Task: task, Code: code, Message: message})
}

// finish ends the run. A batch failure marks every file not yet printed
// with ok:false and a batch error; every remaining file is then flushed in
// input order.
func (c *collector) finish(err error) {
	code, message := "", ""
	switch {
	case c.batchErr != nil:
		code, message = c.batchErr.code, c.batchErr.message
	case err != nil:
		code, message = "engine_crashed", err.Error()
	}
	if code != "" {
		for i := range c.items {
			if !c.printed[i] {
				c.addError(i, "batch", code, message)
			}
		}
	}
	for i := range c.items {
		c.ready[i] = true
	}
	c.flush()
}

func (c *collector) flush() {
	for c.next < len(c.items) && c.ready[c.next] {
		i := c.next
		c.next++
		c.printed[i] = true
		b, err := json.Marshal(c.items[i])
		if err != nil {
			continue
		}
		_, _ = c.out.Write(append(b, '\n'))
	}
}

// exitCode is 0 when every file is ok and 1 when any failed.
func (c *collector) exitCode() int {
	for _, r := range c.items {
		if !r.OK {
			return 1
		}
	}
	return 0
}

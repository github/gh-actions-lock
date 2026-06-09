// Package profile captures phase timing, CPU profiles, and HTTP
// round-trip logs for performance analysis.
package profile

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/pprof"
	"runtime/trace"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type gqlLabelKeyT struct{}

var gqlLabelKey gqlLabelKeyT

// WithGraphQLLabel returns a context that tags subsequent HTTP requests
// with a GraphQL operation label (e.g. "resolve", "reachability", "peel").
// The loggingTransport uses this to break down POST /graphql by purpose.
func WithGraphQLLabel(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, gqlLabelKey, label)
}

// GraphQLLabel extracts the operation label from ctx, or "" if absent.
func GraphQLLabel(ctx context.Context) string {
	if v, ok := ctx.Value(gqlLabelKey).(string); ok {
		return v
	}
	return ""
}

// Session holds all profiling state for one CLI invocation.
// Create with Start, tear down with Stop (writes summaries).
type Session struct {
	traceFile *os.File
	cpuFile   *os.File
	w         io.Writer // summary output (stderr)
	spans     []span
	mu        sync.Mutex
	httpLog   *HTTPLog
	startTime time.Time
}

type span struct {
	name     string
	start    time.Time
	duration time.Duration
}

// Options configures what profiling to enable.
type Options struct {
	TracePath      string // runtime/trace output file (viewable with `go tool trace`)
	CPUProfilePath string // pprof CPU profile output file
	HTTPLog        bool   // log every HTTP round-trip
	Output         io.Writer
}

// Start begins profiling. Returns a no-op session if nothing is enabled.
func Start(opts Options) (*Session, error) {
	s := &Session{
		w:         opts.Output,
		startTime: time.Now(),
	}
	if s.w == nil {
		s.w = os.Stderr
	}

	if opts.TracePath != "" {
		f, err := os.Create(opts.TracePath)
		if err != nil {
			return nil, fmt.Errorf("creating trace file: %w", err)
		}
		s.traceFile = f
		if err := trace.Start(f); err != nil {
			f.Close()
			return nil, fmt.Errorf("starting trace: %w", err)
		}
	}

	if opts.CPUProfilePath != "" {
		f, err := os.Create(opts.CPUProfilePath)
		if err != nil {
			s.cleanupTrace()
			return nil, fmt.Errorf("creating cpu profile: %w", err)
		}
		s.cpuFile = f
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			s.cleanupTrace()
			return nil, fmt.Errorf("starting cpu profile: %w", err)
		}
	}

	if opts.HTTPLog {
		s.httpLog = &HTTPLog{}
	}

	return s, nil
}

// Phase records wall-clock timing for a named phase. Call the returned
// function when the phase ends.
func (s *Session) Phase(name string) func() {
	if s == nil {
		return func() {}
	}
	start := time.Now()
	return func() {
		dur := time.Since(start)
		s.mu.Lock()
		s.spans = append(s.spans, span{name: name, start: start, duration: dur})
		s.mu.Unlock()
	}
}

// WrapTransport wraps an http.RoundTripper with request logging.
// Returns the original transport if HTTP logging is disabled.
func (s *Session) WrapTransport(rt http.RoundTripper) http.RoundTripper {
	if s == nil || s.httpLog == nil {
		return rt
	}
	return &loggingTransport{inner: rt, log: s.httpLog}
}

// cleanupTrace stops and closes the trace file on error paths during Start.
func (s *Session) cleanupTrace() {
	if s.traceFile != nil {
		trace.Stop()
		s.traceFile.Close()
		s.traceFile = nil
	}
}

// Enabled reports whether any profiling is active.
func (s *Session) Enabled() bool {
	return s != nil && (s.traceFile != nil || s.cpuFile != nil || s.httpLog != nil)
}

// Stop ends profiling, writes summary, closes files.
func (s *Session) Stop() {
	if s == nil {
		return
	}
	var cpuPath, tracePath string
	if s.cpuFile != nil {
		pprof.StopCPUProfile()
		cpuPath = s.cpuFile.Name()
		s.cpuFile.Close()
		fmt.Fprintf(s.w, "  cpu profile: %s\n", cpuPath)
	}
	if s.traceFile != nil {
		trace.Stop()
		tracePath = s.traceFile.Name()
		s.traceFile.Close()
		s.traceFile = nil
		fmt.Fprintf(s.w, "  trace:       %s\n", tracePath)
	}

	if len(s.spans) > 0 || (s.httpLog != nil && s.httpLog.Total() > 0) {
		fmt.Fprintf(s.w, "\n── profile summary (%s total) ──\n", time.Since(s.startTime).Round(time.Millisecond))
	}

	if len(s.spans) > 0 {
		fmt.Fprintf(s.w, "\nPhases:\n")
		for _, sp := range s.spans {
			fmt.Fprintf(s.w, "  %-40s %s\n", sp.name, sp.duration.Round(time.Millisecond))
		}
	}

	if s.httpLog != nil && s.httpLog.Total() > 0 {
		s.httpLog.WriteSummary(s.w)
	}

	// Print ready-to-paste commands for interactive visualization.
	if cpuPath != "" || tracePath != "" {
		fmt.Fprintf(s.w, "\nVisualize:\n")
		if cpuPath != "" {
			fmt.Fprintf(s.w, "  go tool pprof -http=:8080 %s\n", cpuPath)
		}
		if tracePath != "" {
			fmt.Fprintf(s.w, "  go tool trace %s\n", tracePath)
		}
	}
}

// ── HTTP logging ──

// HTTPLog collects per-request timing data.
type HTTPLog struct {
	mu      sync.Mutex
	entries []httpEntry
	total   atomic.Int64
}

type httpEntry struct {
	method   string
	path     string
	gqlLabel string
	status   int
	duration time.Duration
	ts       time.Time
}

// Total returns the number of HTTP round trips recorded.
func (h *HTTPLog) Total() int64 { return h.total.Load() }

func (h *HTTPLog) record(e httpEntry) {
	h.total.Add(1)
	h.mu.Lock()
	h.entries = append(h.entries, e)
	h.mu.Unlock()
}

// WriteSummary prints aggregated HTTP stats.
func (h *HTTPLog) WriteSummary(w io.Writer) {
	h.mu.Lock()
	entries := make([]httpEntry, len(h.entries))
	copy(entries, h.entries)
	h.mu.Unlock()

	if len(entries) == 0 {
		return
	}

	// Aggregate by path pattern.
	type bucket struct {
		pattern string
		count   int
		total   time.Duration
		min     time.Duration
		max     time.Duration
		errors  int
	}
	byPattern := map[string]*bucket{}
	var totalDuration time.Duration
	var totalErrors int

	for _, e := range entries {
		pat := classifyPath(e.method, e.path, e.gqlLabel)
		b, ok := byPattern[pat]
		if !ok {
			b = &bucket{pattern: pat, min: e.duration}
			byPattern[pat] = b
		}
		b.count++
		b.total += e.duration
		if e.duration < b.min {
			b.min = e.duration
		}
		if e.duration > b.max {
			b.max = e.duration
		}
		if e.status >= 400 {
			b.errors++
			totalErrors++
		}
		totalDuration += e.duration
	}

	sorted := make([]*bucket, 0, len(byPattern))
	for _, b := range byPattern {
		sorted = append(sorted, b)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].total > sorted[j].total })

	fmt.Fprintf(w, "\nHTTP requests: %d total, %d errors, %s cumulative\n\n",
		len(entries), totalErrors, totalDuration.Round(time.Millisecond))
	fmt.Fprintf(w, "  %-50s %5s %8s %8s %8s %8s\n", "ENDPOINT", "COUNT", "TOTAL", "AVG", "MIN", "MAX")
	fmt.Fprintf(w, "  %s\n", strings.Repeat("─", 100))
	for _, b := range sorted {
		avg := b.total / time.Duration(b.count)
		errSuffix := ""
		if b.errors > 0 {
			errSuffix = fmt.Sprintf(" (%d err)", b.errors)
		}
		fmt.Fprintf(w, "  %-50s %5d %8s %8s %8s %8s%s\n",
			b.pattern, b.count,
			b.total.Round(time.Millisecond),
			avg.Round(time.Millisecond),
			b.min.Round(time.Millisecond),
			b.max.Round(time.Millisecond),
			errSuffix)
	}
}

// classifyPath normalizes API paths into patterns for aggregation.
func classifyPath(method, path, gqlLabel string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	// GraphQL: break down by operation label when available.
	if method == "POST" && path == "/graphql" && gqlLabel != "" {
		return "POST /graphql (" + gqlLabel + ")"
	}
	if len(parts) < 3 {
		return method + " " + path
	}
	// repos/{owner}/{repo}/... → repos/*/compare, repos/*/branches, etc.
	if parts[0] == "repos" && len(parts) >= 4 {
		nwo := parts[1] + "/" + parts[2]
		// Collapse SHA-like segments.
		normalized := make([]string, 0, len(parts)-3)
		for _, p := range parts[3:] {
			if len(p) >= 40 && isHex(p) {
				normalized = append(normalized, "{sha}")
			} else {
				normalized = append(normalized, p)
			}
		}
		return fmt.Sprintf("%s repos/%s/%s", method, nwo, strings.Join(normalized, "/"))
	}
	return method + " " + path
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// loggingTransport wraps an http.RoundTripper to log every request.
type loggingTransport struct {
	inner http.RoundTripper
	log   *HTTPLog
}

func (lt *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := lt.inner.RoundTrip(req)
	dur := time.Since(start)

	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	lt.log.record(httpEntry{
		method:   req.Method,
		path:     req.URL.Path,
		gqlLabel: GraphQLLabel(req.Context()),
		status:   status,
		duration: dur,
		ts:       start,
	})
	return resp, err
}

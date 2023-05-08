//  Copyright 2023 Stealth Rocket, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package wzprof

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// CPUProfiler is the implementation of a performance profiler recording
// samples of CPU time spent in functions of a WebAssembly module.
//
// The profiler generates samples of two types:
// - "cpu" records the time spent in function calls (in nanoseconds).
// - "sample" counts the number of function calls.
type CPUProfiler struct {
	mutex  sync.Mutex
	counts stackCounterMap
	frames []cpuTimeFrame
	traces []stackTrace
	time   func() int64
	start  time.Time
	host   bool
}

// CPUProfilerOption is a type used to represent configuration options for
// CPUProfiler instances created by NewCPUProfiler.
type CPUProfilerOption func(*CPUProfiler)

// EnableHostTime confiures a CPU time profiler to acount for time spent
// in calls to host functions.
//
// Default to false.
func EnableHostTime(enable bool) CPUProfilerOption {
	return func(p *CPUProfiler) { p.host = enable }
}

// TimeFunc configures the time function used by the CPU profiler to collect
// monotonic timestamps.
//
// By default, the system's monotonic time is used.
func TimeFunc(time func() int64) CPUProfilerOption {
	return func(p *CPUProfiler) { p.time = time }
}

type cpuTimeFrame struct {
	start int64
	trace stackTrace
}

// NewCPUProfiler constructs a new instance of CPUProfiler using the
// given time function to record the CPU time consumed.
func NewCPUProfiler(options ...CPUProfilerOption) *CPUProfiler {
	p := &CPUProfiler{
		time: nanotime,
	}
	for _, opt := range options {
		opt(p)
	}
	return p
}

// StartProfile begins recording the CPU profile. The method returns a boolean
// to indicate whether starting the profile suceeded (e.g. false is returned if
// it was already started).
func (p *CPUProfiler) StartProfile() bool {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.counts != nil {
		return false // already started
	}

	p.counts = make(stackCounterMap)
	p.start = time.Now()
	return true
}

// StopProfile stops recording and returns the CPU profile. The method returns
// nil if recording of the CPU profile wasn't started.
func (p *CPUProfiler) StopProfile(sampleRate float64, symbols Symbolizer) *profile.Profile {
	p.mutex.Lock()
	samples, start := p.counts, p.start
	p.counts = nil
	p.mutex.Unlock()

	if samples == nil {
		return nil
	}

	duration := time.Since(start)

	if !p.host {
		for k, sample := range samples {
			if sample.stack.host() {
				delete(samples, k)
				for _, other := range samples {
					if sample.stack.contains(other.stack) {
						other.subtract(sample.total())
					}
				}
			}
		}
	}

	return buildProfile(sampleRate, symbols, samples, start, duration,
		[]*profile.ValueType{
			{Type: "cpu", Unit: "nanoseconds"},
			{Type: "samples", Unit: "count"},
		},
	)
}

// NewHandler returns a http handler allowing the profiler to be exposed on a
// pprof-compatible http endpoint.
//
// The sample rate is a value between 0 and 1 used to scale the profile results
// based on the sampling rate applied to the profiler so the resulting values
// remain representative.
//
// The symbolizer passed as argument is used to resolve names of program
// locations recorded in the profile.
func (p *CPUProfiler) NewHandler(sampleRate float64, symbols Symbolizer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		duration := 30 * time.Second

		if seconds := r.FormValue("seconds"); seconds != "" {
			n, err := strconv.ParseInt(seconds, 10, 64)
			if err == nil && n > 0 {
				duration = time.Duration(n) * time.Second
			}
		}

		ctx := r.Context()
		deadline, ok := ctx.Deadline()
		if ok {
			if timeout := time.Until(deadline); duration > timeout {
				serveError(w, http.StatusBadRequest, "profile duration exceeds server's WriteTimeout")
				return
			}
		}

		if !p.StartProfile() {
			serveError(w, http.StatusInternalServerError, "Could not enable CPU profiling: profiler already running")
			return
		}

		timer := time.NewTimer(duration)
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		serveProfile(w, p.StopProfile(sampleRate, symbols))
	})
}

// NewListener returns a function listener suited to record CPU timings of
// calls to the function passed as argument.
func (p *CPUProfiler) NewListener(def api.FunctionDefinition) experimental.FunctionListener {
	return cpuListener{p}
}

type cpuListener struct{ *CPUProfiler }

func (p cpuListener) Before(ctx context.Context, mod api.Module, def api.FunctionDefinition, params []uint64, si experimental.StackIterator) context.Context {
	var frame cpuTimeFrame
	p.mutex.Lock()

	if p.counts != nil {
		start := p.time()
		trace := stackTrace{}

		if i := len(p.traces); i > 0 {
			i--
			trace = p.traces[i]
			p.traces = p.traces[:i]
		}

		frame = cpuTimeFrame{
			start: start,
			trace: makeStackTrace(trace, si),
		}
	}

	p.mutex.Unlock()
	p.frames = append(p.frames, frame)
	return ctx
}

func (p cpuListener) After(ctx context.Context, mod api.Module, def api.FunctionDefinition, err error, results []uint64) {
	i := len(p.frames) - 1
	f := p.frames[i]
	p.frames = p.frames[:i]

	if f.start != 0 {
		p.mutex.Lock()
		if p.counts != nil {
			p.counts.observe(f.trace, p.time()-f.start)
		}
		p.mutex.Unlock()
		p.traces = append(p.traces, f.trace)
	}
}

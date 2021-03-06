// Copyright 2019 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package exec

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/grailbio/bigslice"
)

// traceEvent is an event in the Chrome tracing format. The fields are
// mirrored exactly. For more details, see:
//	https://docs.google.com/document/d/1CvAClvFfyA5R-PhYUmn5OOQtYMH4h6I0nSsKchNAySU/preview
type traceEvent struct {
	Pid  int                    `json:"pid"`
	Tid  int                    `json:"tid"`
	Ts   int64                  `json:"ts"`
	Ph   string                 `json:"ph"`
	Dur  int64                  `json:"dur,omitempty"`
	Name string                 `json:"name"`
	Cat  string                 `json:"cat,omitempty"`
	Args map[string]interface{} `json:"args"`
}

// A tracer tracks a set of trace events associated with objects in
// Bigslice. Trace events are logged in the Chrome tracing format and
// can be visualized using its built-in visualization tool
// (chrome://tracing). Each machine is represented as a Chrome "process",
// and individual task or invocation events are tracked by the machine
// they are run on.
//
// Tracer does not assign "thread IDs" to trace events; rather events are
// coalesced into "complete events" (X) at the time of rendering.
//
// TODO(marius): garbage collection of old events.
type tracer struct {
	mu sync.Mutex

	events        []traceEvent
	taskEvents    map[*Task][]traceEvent
	compileEvents map[uint64][]traceEvent

	machinePids map[*sliceMachine]int

	// firstEvent is used to store the time of the first observed
	// event so that the offsets in the trace are meaningful.
	firstEvent time.Time
}

func newTracer() *tracer {
	return &tracer{
		taskEvents:    make(map[*Task][]traceEvent),
		compileEvents: make(map[uint64][]traceEvent),
		machinePids:   make(map[*sliceMachine]int),
	}
}

// Event logs an event on the provided machine with the given
// subject, type (ph), and arguments. The event's subject must be
// either a *Task or a bigslice.Invocation; ph is as in Chrome's
// tracing format. Arguments is list of interleaved key-value pairs
// that are attached as event metadata. Args must be of even length.
//
// If mach is nil, the event is assigned to the evaluator.
func (t *tracer) Event(mach *sliceMachine, subject interface{}, ph string, args ...interface{}) {
	if t == nil {
		return
	}
	if len(args)%2 != 0 {
		panic("trace.Event: invalid arguments")
	}
	var event traceEvent
	event.Args = make(map[string]interface{}, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		event.Args[fmt.Sprint(args[i])] = args[i+1]
	}
	event.Ph = ph
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.firstEvent.IsZero() {
		t.firstEvent = time.Now()
		event.Ts = 0
	} else {
		event.Ts = time.Since(t.firstEvent).Nanoseconds() / 1e3
	}
	if mach != nil {
		pid, ok := t.machinePids[mach]
		if !ok {
			pid = len(t.machinePids) + 1 // pid=0 is reserved for evaluator events
			t.machinePids[mach] = pid
			// Attach "process" name metadata so we can identify where a task is running.
			t.events = append(t.events, traceEvent{
				Pid:  pid,
				Ts:   event.Ts,
				Ph:   "M",
				Name: "process_name",
				Args: map[string]interface{}{
					"name": mach.Addr,
				},
			})
		}
		event.Pid = pid
	}
	event.Tid = 1
	switch arg := subject.(type) {
	case *Task:
		event.Name = arg.Name.String()
		event.Cat = "task"
		t.taskEvents[arg] = append(t.taskEvents[arg], event)
	case bigslice.Invocation:
		var name strings.Builder
		fmt.Fprint(&name, arg.Index)
		if arg.Exclusive {
			name.WriteString("[x]")
		}
		event.Name = name.String()
		event.Cat = "invocation"
		t.compileEvents[arg.Index] = append(t.compileEvents[arg.Index], event)
	default:
		panic(fmt.Sprintf("unsupported subject type %T", subject))
	}
}

// Marshal writes the trace captured by t into the writer w in
// Chrome's event tracing format.
func (t *tracer) Marshal(w io.Writer) error {
	t.mu.Lock()
	events := make([]traceEvent, len(t.events))
	copy(events, t.events)
	for _, v := range t.taskEvents {
		events = appendCoalesce(events, v, t.firstEvent)
	}
	for _, v := range t.compileEvents {
		events = appendCoalesce(events, v, t.firstEvent)
	}
	t.mu.Unlock()

	envelope := struct {
		TraceEvents []traceEvent `json:"traceEvents"`
	}{events}
	enc := json.NewEncoder(w)
	return enc.Encode(envelope)
}

// appendCoalesce appends a set of events on the provided list,
// first coalescing events so that "B" and "E" events are matched
// into a single "X" event. This produces more visually compact (and
// useful) trace visualizations. appendCoalesce also prunes orphan
// events.
func appendCoalesce(list []traceEvent, events []traceEvent, firstEvent time.Time) []traceEvent {
	var begIndex = -1
	for _, event := range events {
		if event.Ph == "B" && begIndex < 0 {
			begIndex = len(list)
		}
		if event.Ph == "E" && begIndex >= 0 {
			list[begIndex].Ph = "X"
			list[begIndex].Dur = event.Ts - list[begIndex].Ts
			if list[begIndex].Dur == 0 {
				list[begIndex].Dur = 1
			}
			for k, v := range event.Args {
				if _, ok := list[begIndex].Args[k]; !ok {
					list[begIndex].Args[k] = v
				}
			}
			// Reset the begin index, so that if a task fails but is retried
			// on the same machine; then these are captured as two unique
			// events.
			begIndex = -1
		} else if event.Ph != "E" {
			list = append(list, event)
		} // drop unmatched "E"s
	}
	if begIndex >= 0 {
		// We have an unmatched "B". Drop it.
		copy(list[begIndex:], list[begIndex+1:])
		list = list[:len(list)-1]
	}
	return list
}

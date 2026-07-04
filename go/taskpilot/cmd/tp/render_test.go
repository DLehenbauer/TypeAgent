package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/telemetry"
)

func TestPrintLogEventRendersRunAndNodeSpans(t *testing.T) {
	now := time.Now()
	end := now.Add(5 * time.Millisecond)

	cases := []struct {
		name string
		rec  telemetry.SpanRecord
		want []string
	}{
		{
			name: "run start",
			rec: telemetry.SpanRecord{
				Phase: telemetry.PhaseStart, Name: telemetry.SpanRun, StartTime: now,
				Attributes: map[string]telemetry.AttrValue{telemetry.AttrRunID: telemetry.StringValue("run-x"), telemetry.AttrEntry: telemetry.StringValue("hello")},
			},
			want: []string{"run started", "entry=hello", "run=run-x"},
		},
		{
			name: "run end ok",
			rec: telemetry.SpanRecord{
				Phase: telemetry.PhaseEnd, Name: telemetry.SpanRun, StartTime: now, EndTime: &end, Status: telemetry.StatusOK,
				Attributes: map[string]telemetry.AttrValue{telemetry.AttrRunID: telemetry.StringValue("run-x")},
			},
			want: []string{"run completed", "run=run-x"},
		},
		{
			name: "run end error",
			rec: telemetry.SpanRecord{
				Phase: telemetry.PhaseEnd, Name: telemetry.SpanRun, StartTime: now, EndTime: &end,
				Status: telemetry.StatusError, StatusMsg: "kaboom",
				Attributes: map[string]telemetry.AttrValue{telemetry.AttrRunID: telemetry.StringValue("run-x")},
			},
			want: []string{"run failed", "error=kaboom"},
		},
		{
			name: "node start",
			rec: telemetry.SpanRecord{
				Phase: telemetry.PhaseStart, Name: telemetry.SpanNode, StartTime: now,
				Attributes: map[string]telemetry.AttrValue{telemetry.AttrNodeName: telemetry.StringValue("render"), telemetry.AttrTask: telemetry.StringValue("template.expand"), telemetry.AttrNodeID: telemetry.StringValue("nid")},
			},
			want: []string{"node started", "render", "task=template.expand", "nodeId=nid"},
		},
		{
			name: "node completed",
			rec: telemetry.SpanRecord{
				Phase: telemetry.PhaseEnd, Name: telemetry.SpanNode, StartTime: now, EndTime: &end, Status: telemetry.StatusOK,
				DurationMs: 5,
				Attributes: map[string]telemetry.AttrValue{telemetry.AttrNodeName: telemetry.StringValue("render"), telemetry.AttrTask: telemetry.StringValue("template.expand"), telemetry.AttrNodeID: telemetry.StringValue("nid"), telemetry.AttrCacheStatus: telemetry.StringValue(telemetry.CacheStatusMiss)},
			},
			want: []string{"node.completed", "render", "cache=miss", "durationMs=5"},
		},
		{
			name: "node failed",
			rec: telemetry.SpanRecord{
				Phase: telemetry.PhaseEnd, Name: telemetry.SpanNode, StartTime: now, EndTime: &end,
				Status: telemetry.StatusError, StatusMsg: "bad input", DurationMs: 2,
				Attributes: map[string]telemetry.AttrValue{telemetry.AttrNodeName: telemetry.StringValue("render"), telemetry.AttrTask: telemetry.StringValue("template.expand"), telemetry.AttrNodeID: telemetry.StringValue("nid"), telemetry.AttrStage: telemetry.StringValue("input_validation")},
			},
			want: []string{"node failed", "stage=input_validation", "error=bad input"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := printLogEvent(&buf, "", tc.rec); err != nil {
				t.Fatalf("printLogEvent: %v", err)
			}
			got := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("render = %q, missing %q", got, want)
				}
			}
		})
	}
}

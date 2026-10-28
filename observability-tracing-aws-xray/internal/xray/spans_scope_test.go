// Copyright 2026 The OpenChoreo Authors
// SPDX-License-Identifier: Apache-2.0

package xray

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/xray"
	xraytypes "github.com/aws/aws-sdk-go-v2/service/xray/types"
)

const scopeTestTraceID = "1-5759e988-bd862e3fe1be46a994272793"

// testIdentity is the OpenChoreo identity of a workload as it appears on the
// spans the collector exports: the namespace and UIDs as indexed annotations,
// the names only in the segment's resource metadata.
type testIdentity struct {
	namespace      string
	project        string
	component      string
	environment    string
	projectUID     string
	componentUID   string
	environmentUID string
}

var (
	identityA1 = testIdentity{"ns-a", "proj-a", "comp-a1", "dev", "puid-a", "cuid-a1", "euid-dev"}
	identityA2 = testIdentity{"ns-a", "proj-a", "comp-a2", "dev", "puid-a", "cuid-a2", "euid-dev"}
	identityA3 = testIdentity{"ns-a", "proj-a2", "comp-a3", "prod", "puid-a2", "cuid-a3", "euid-prod"}
	identityB  = testIdentity{"ns-b", "proj-b", "comp-b", "dev", "puid-b", "cuid-b", "euid-dev-b"}
)

// annotations leaves out empty fields, as the collector transform does for a
// resource attribute that is not set.
func (id testIdentity) annotations() map[string]interface{} {
	return withoutEmpty(map[string]interface{}{
		"openchoreo.dev_namespace":       id.namespace,
		"openchoreo.dev_project_uid":     id.projectUID,
		"openchoreo.dev_component_uid":   id.componentUID,
		"openchoreo.dev_environment_uid": id.environmentUID,
	})
}

func withoutEmpty(m map[string]interface{}) map[string]interface{} {
	for k, v := range m {
		if v == "" {
			delete(m, k)
		}
	}
	return m
}

// metadata mirrors what the awsxray exporter writes for a segment: every
// non-indexed resource attribute under metadata.default."otel.resource.<key>".
func (id testIdentity) metadata() map[string]interface{} {
	return map[string]interface{}{
		"default": withoutEmpty(map[string]interface{}{
			"otel.resource.openchoreo.dev/namespace":       id.namespace,
			"otel.resource.openchoreo.dev/project":         id.project,
			"otel.resource.openchoreo.dev/component":       id.component,
			"otel.resource.openchoreo.dev/environment":     id.environment,
			"otel.resource.openchoreo.dev/project-uid":     id.projectUID,
			"otel.resource.openchoreo.dev/component-uid":   id.componentUID,
			"otel.resource.openchoreo.dev/environment-uid": id.environmentUID,
			"otel.resource.service.name":                   id.component + "-svc",
		}),
	}
}

func testSegment(id, name string, annotations, metadata map[string]interface{}, subsegments ...map[string]interface{}) map[string]interface{} {
	seg := map[string]interface{}{
		"id":         id,
		"name":       name,
		"trace_id":   scopeTestTraceID,
		"start_time": 1465510988.0,
		"end_time":   1465510988.5,
		"origin":     "AWS::EKS::Container",
		"aws":        map[string]interface{}{"xray": map[string]interface{}{"sdk": "opentelemetry"}},
	}
	if annotations != nil {
		seg["annotations"] = annotations
	}
	if metadata != nil {
		seg["metadata"] = metadata
	}
	if len(subsegments) > 0 {
		seg["subsegments"] = subsegments
	}
	return seg
}

func testSubsegment(id string, annotations map[string]interface{}, subsegments ...map[string]interface{}) map[string]interface{} {
	sub := map[string]interface{}{
		"id":         id,
		"name":       id + "-call",
		"namespace":  "remote",
		"start_time": 1465510988.1,
		"end_time":   1465510988.3,
	}
	if annotations != nil {
		sub["annotations"] = annotations
	}
	if len(subsegments) > 0 {
		sub["subsegments"] = subsegments
	}
	return sub
}

func newTraceClient(t *testing.T, segments ...map[string]interface{}) *Client {
	t.Helper()
	trace := xraytypes.Trace{Id: aws.String(scopeTestTraceID)}
	for _, seg := range segments {
		doc, err := json.Marshal(seg)
		if err != nil {
			t.Fatalf("marshal segment: %v", err)
		}
		trace.Segments = append(trace.Segments, xraytypes.Segment{Document: aws.String(string(doc))})
	}
	mock := &mockXRayClient{
		batchGetTracesFn: func(ctx context.Context, input *xray.BatchGetTracesInput, opts ...func(*xray.Options)) (*xray.BatchGetTracesOutput, error) {
			return &xray.BatchGetTracesOutput{Traces: []xraytypes.Trace{trace}}, nil
		},
	}
	return NewClientWithAWS(mock, &mockSTSClient{}, testLogger())
}

// mixedScopeTrace is one trace that crosses three workloads in namespace A and
// one in namespace B, each segment with a nested subsegment of its own.
func mixedScopeTrace() []map[string]interface{} {
	return []map[string]interface{}{
		testSegment("segA1", "a1", identityA1.annotations(), identityA1.metadata(),
			testSubsegment("subA1", identityA1.annotations())),
		testSegment("segB", "b", identityB.annotations(), identityB.metadata(),
			testSubsegment("subB", identityB.annotations())),
		testSegment("segA2", "a2", identityA2.annotations(), identityA2.metadata(),
			testSubsegment("subA2", identityA2.annotations())),
		testSegment("segA3", "a3", identityA3.annotations(), identityA3.metadata(),
			testSubsegment("subA3", identityA3.annotations())),
	}
}

func spanIDs(spans []SpanEntry) []string {
	ids := make([]string, 0, len(spans))
	for _, s := range spans {
		ids = append(ids, s.SpanID)
	}
	sort.Strings(ids)
	return ids
}

func equalIDs(got, want []string) bool {
	sort.Strings(want)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestGetSpans_ForeignTraceReturnsNothing(t *testing.T) {
	for _, includeAttributes := range []bool{false, true} {
		client := newTraceClient(t,
			testSegment("segB", "b", identityB.annotations(), identityB.metadata(),
				testSubsegment("subB", identityB.annotations())),
		)
		result, err := client.GetSpans(context.Background(), TracesQueryParams{
			TraceID:           "5759e988bd862e3fe1be46a994272793",
			Scope:             Scope{Namespace: "ns-a"},
			IncludeAttributes: includeAttributes,
		})
		if err != nil {
			t.Fatalf("includeAttributes=%v: unexpected error: %v", includeAttributes, err)
		}
		if len(result.Spans) != 0 || result.Total != 0 {
			t.Errorf("includeAttributes=%v: expected no spans and total 0, got %v total %d",
				includeAttributes, spanIDs(result.Spans), result.Total)
		}
	}
}

func TestGetSpans_MixedScopeTraceKeepsOnlyMatchingSpans(t *testing.T) {
	tests := []struct {
		name  string
		scope Scope
		want  []string
	}{
		{"namespace", Scope{Namespace: "ns-a"}, []string{"segA1", "subA1", "segA2", "subA2", "segA3", "subA3"}},
		{"project", Scope{Namespace: "ns-a", ProjectID: "puid-a"}, []string{"segA1", "subA1", "segA2", "subA2"}},
		{"component", Scope{Namespace: "ns-a", ComponentID: "cuid-a1"}, []string{"segA1", "subA1"}},
		{"environment", Scope{Namespace: "ns-a", EnvironmentID: "euid-prod"}, []string{"segA3", "subA3"}},
		{"all fields", Scope{Namespace: "ns-a", ProjectID: "puid-a", ComponentID: "cuid-a2", EnvironmentID: "euid-dev"}, []string{"segA2", "subA2"}},
		{"component of another namespace", Scope{Namespace: "ns-a", ComponentID: "cuid-b"}, nil},
		{"environment mismatch", Scope{Namespace: "ns-a", ComponentID: "cuid-a1", EnvironmentID: "euid-prod"}, nil},
		{"other namespace", Scope{Namespace: "ns-c"}, nil},
	}

	for _, tt := range tests {
		for _, includeAttributes := range []bool{false, true} {
			client := newTraceClient(t, mixedScopeTrace()...)
			result, err := client.GetSpans(context.Background(), TracesQueryParams{
				TraceID:           "5759e988bd862e3fe1be46a994272793",
				Scope:             tt.scope,
				IncludeAttributes: includeAttributes,
			})
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tt.name, err)
			}
			got := spanIDs(result.Spans)
			if !equalIDs(got, append([]string(nil), tt.want...)) {
				t.Errorf("%s (includeAttributes=%v): expected spans %v, got %v", tt.name, includeAttributes, tt.want, got)
			}
			if result.Total != len(tt.want) {
				t.Errorf("%s (includeAttributes=%v): expected total %d, got %d", tt.name, includeAttributes, len(tt.want), result.Total)
			}
			for _, s := range result.Spans {
				if v := s.Attributes["annotation.openchoreo.dev_namespace"]; v != nil && v != tt.scope.Namespace {
					t.Errorf("%s: span %s leaks foreign namespace annotation %v", tt.name, s.SpanID, v)
				}
				if v := s.ResourceAttributes["openchoreo.dev/namespace"]; v != nil && v != tt.scope.Namespace {
					t.Errorf("%s: span %s leaks foreign namespace resource %v", tt.name, s.SpanID, v)
				}
			}
		}
	}
}

func TestGetSpans_SubsegmentJudgedOnOwnAnnotations(t *testing.T) {
	client := newTraceClient(t,
		testSegment("segA1", "a1", identityA1.annotations(), identityA1.metadata(),
			testSubsegment("subB", identityB.annotations(),
				testSubsegment("subA1", identityA1.annotations())),
			testSubsegment("subNone", nil)),
	)
	result, err := client.GetSpans(context.Background(), TracesQueryParams{
		TraceID: "5759e988bd862e3fe1be46a994272793",
		Scope:   Scope{Namespace: "ns-a"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// subB is excluded on its own annotations, but its in-scope child is still
	// returned; subNone carries no identity and is excluded.
	if got, want := spanIDs(result.Spans), []string{"segA1", "subA1"}; !equalIDs(got, want) {
		t.Errorf("expected spans %v, got %v", want, got)
	}
}

func TestGetSpans_MissingOrInvalidIdentityExcluded(t *testing.T) {
	nonString := identityA1.annotations()
	nonString["openchoreo.dev_namespace"] = 42
	missingUID := identityA1.annotations()
	delete(missingUID, "openchoreo.dev_component_uid")

	client := newTraceClient(t,
		testSegment("segNone", "none", nil, nil),
		testSegment("segOther", "other", map[string]interface{}{"key1": "value1"}, nil),
		testSegment("segNonString", "nonstring", nonString, nil),
		testSegment("segMissingUID", "missinguid", missingUID, nil),
	)

	for _, scope := range []Scope{
		{Namespace: "ns-a"},
		{Namespace: "ns-a", ComponentID: "cuid-a1"},
	} {
		result, err := client.GetSpans(context.Background(), TracesQueryParams{
			TraceID: "5759e988bd862e3fe1be46a994272793",
			Scope:   scope,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var want []string
		if scope.ComponentID == "" {
			// Only the component UID is missing, so a namespace-only scope matches.
			want = []string{"segMissingUID"}
		}
		if got := spanIDs(result.Spans); !equalIDs(got, want) {
			t.Errorf("scope %+v: expected spans %v, got %v", scope, want, got)
		}
	}
}

func TestGetSpans_EmptyNamespaceScopeFailsClosed(t *testing.T) {
	client := newTraceClient(t, mixedScopeTrace()...)
	result, err := client.GetSpans(context.Background(), TracesQueryParams{
		TraceID: "5759e988bd862e3fe1be46a994272793",
		Scope:   Scope{ComponentID: "cuid-a1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Spans) != 0 || result.Total != 0 {
		t.Errorf("expected no spans without a namespace scope, got %v total %d", spanIDs(result.Spans), result.Total)
	}
}

func TestGetSpans_TotalCountsMatchesBeforeLimit(t *testing.T) {
	client := newTraceClient(t, mixedScopeTrace()...)
	result, err := client.GetSpans(context.Background(), TracesQueryParams{
		TraceID: "5759e988bd862e3fe1be46a994272793",
		Scope:   Scope{Namespace: "ns-a", ProjectID: "puid-a"},
		Limit:   3,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Total != 4 {
		t.Errorf("expected total 4 (matching spans before limit), got %d", result.Total)
	}
	if len(result.Spans) != 3 {
		t.Fatalf("expected 3 spans after limit, got %d", len(result.Spans))
	}
	for _, s := range result.Spans {
		switch s.SpanID {
		case "segA1", "subA1", "segA2", "subA2":
		default:
			t.Errorf("limit applied before filter: got out-of-scope span %s", s.SpanID)
		}
	}
}

func assertResourceAttrs(t *testing.T, label string, attrs map[string]interface{}, want map[string]string, absent ...string) {
	t.Helper()
	for k, v := range want {
		if got := attrs[k]; got != v {
			t.Errorf("%s: expected resource attribute %s=%q, got %#v", label, k, v, got)
		}
	}
	for _, k := range absent {
		if got, ok := attrs[k]; ok {
			t.Errorf("%s: expected resource attribute %s to be absent, got %#v", label, k, got)
		}
	}
}

func fullResource(id testIdentity) map[string]string {
	return map[string]string{
		"openchoreo.dev/namespace":       id.namespace,
		"openchoreo.dev/project":         id.project,
		"openchoreo.dev/component":       id.component,
		"openchoreo.dev/environment":     id.environment,
		"openchoreo.dev/project-uid":     id.projectUID,
		"openchoreo.dev/component-uid":   id.componentUID,
		"openchoreo.dev/environment-uid": id.environmentUID,
	}
}

func identityOnlyResource(id testIdentity) map[string]string {
	return map[string]string{
		"openchoreo.dev/namespace":       id.namespace,
		"openchoreo.dev/project-uid":     id.projectUID,
		"openchoreo.dev/component-uid":   id.componentUID,
		"openchoreo.dev/environment-uid": id.environmentUID,
	}
}

var resourceNameKeys = []string{"openchoreo.dev/project", "openchoreo.dev/component", "openchoreo.dev/environment"}

// inheritanceTrace holds a segment in A1 with a matching subsegment (and a
// matching grandchild), a subsegment that belongs to B and a subsegment that
// carries no OpenChoreo identity.
func inheritanceTrace() []map[string]interface{} {
	return []map[string]interface{}{
		testSegment("segA1", "a1", identityA1.annotations(), identityA1.metadata(),
			testSubsegment("subA1", identityA1.annotations(),
				testSubsegment("subsubA1", identityA1.annotations())),
			testSubsegment("subB", identityB.annotations()),
			testSubsegment("subNone", nil)),
	}
}

func TestGetSpans_ResourceAttributesFromMetadata(t *testing.T) {
	client := newTraceClient(t, inheritanceTrace()...)
	result, err := client.GetSpans(context.Background(), TracesQueryParams{
		TraceID:           "5759e988bd862e3fe1be46a994272793",
		Scope:             Scope{Namespace: "ns-a"},
		IncludeAttributes: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byID := map[string]SpanEntry{}
	for _, s := range result.Spans {
		byID[s.SpanID] = s
	}
	for _, id := range []string{"segA1", "subA1", "subsubA1"} {
		s, ok := byID[id]
		if !ok {
			t.Fatalf("expected span %s in result, got %v", id, spanIDs(result.Spans))
		}
		want := fullResource(identityA1)
		want["service.name"] = "comp-a1-svc"
		if id == "segA1" {
			// aws.* comes from the span's own document, not the enclosing segment.
			want["aws.xray.sdk"] = "opentelemetry"
		}
		assertResourceAttrs(t, id, s.ResourceAttributes, want)
	}
}

func TestGetSpanDetail_ResourceAttributes(t *testing.T) {
	client := newTraceClient(t, inheritanceTrace()...)

	tests := []struct {
		spanID string
		want   map[string]string
		absent []string
	}{
		{"segA1", fullResource(identityA1), nil},
		{"subA1", fullResource(identityA1), nil},
		{"subsubA1", fullResource(identityA1), nil},
		// A subsegment of another workload must not inherit A1's names: it
		// carries only its own namespace and UIDs.
		{"subB", identityOnlyResource(identityB), append([]string{"service.name"}, resourceNameKeys...)},
		{"subNone", nil, append([]string{"openchoreo.dev/namespace", "service.name"}, resourceNameKeys...)},
	}

	for _, tt := range tests {
		result, err := client.GetSpanDetail(context.Background(), TracesQueryParams{
			TraceID: "5759e988bd862e3fe1be46a994272793",
			SpanID:  tt.spanID,
		})
		if tt.want == nil {
			// No OpenChoreo identity: the adapter reports the span as missing.
			if !errors.Is(err, ErrSpanNotFound) {
				t.Errorf("%s: expected ErrSpanNotFound for a span without OpenChoreo identity, got %v", tt.spanID, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tt.spanID, err)
		}
		assertResourceAttrs(t, tt.spanID, result.Span.ResourceAttributes, tt.want, tt.absent...)
		if _, ok := result.Span.ResourceAttributes["aws.xray.sdk"]; tt.spanID == "segA1" && !ok {
			t.Errorf("%s: expected aws.* resource attributes to be kept", tt.spanID)
		}
	}
}

func TestGetSpanDetail_MissingNamesStayAbsent(t *testing.T) {
	// A segment whose resource metadata carries no names (for example data
	// exported without the k8sattributes name labels) keeps only its identity.
	client := newTraceClient(t,
		testSegment("segA1", "a1", identityA1.annotations(), nil),
	)
	result, err := client.GetSpanDetail(context.Background(), TracesQueryParams{
		TraceID: "5759e988bd862e3fe1be46a994272793",
		SpanID:  "segA1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertResourceAttrs(t, "segA1", result.Span.ResourceAttributes, identityOnlyResource(identityA1), resourceNameKeys...)
}

// ownIdentityResource is the identity a span carries from its own annotations,
// with the keys of its empty fields reported as absent.
func ownIdentityResource(id testIdentity) (map[string]string, []string) {
	want := map[string]string{}
	var absent []string
	for k, v := range identityOnlyResource(id) {
		if v == "" {
			absent = append(absent, k)
		} else {
			want[k] = v
		}
	}
	return want, absent
}

func TestGetSpanDetail_ContradictingOrIncompleteMetadataNotTrusted(t *testing.T) {
	tests := []struct {
		name     string
		metadata testIdentity
	}{
		{"other namespace", testIdentity{"ns-b", "proj-a", "comp-a1", "dev", "puid-a", "cuid-a1", "euid-dev"}},
		{"other project uid", testIdentity{"ns-a", "proj-a", "comp-a1", "dev", "puid-x", "cuid-a1", "euid-dev"}},
		{"other component uid", testIdentity{"ns-a", "proj-a", "comp-a1", "dev", "puid-a", "cuid-x", "euid-dev"}},
		{"other environment uid", testIdentity{"ns-a", "proj-a", "comp-a1", "dev", "puid-a", "cuid-a1", "euid-x"}},
		{"metadata without namespace", testIdentity{"", "proj-a", "comp-a1", "dev", "puid-a", "cuid-a1", "euid-dev"}},
		{"metadata without project uid", testIdentity{"ns-a", "proj-a", "comp-a1", "dev", "", "cuid-a1", "euid-dev"}},
		{"metadata without component uid", testIdentity{"ns-a", "proj-a", "comp-a1", "dev", "puid-a", "", "euid-dev"}},
		{"metadata without environment uid", testIdentity{"ns-a", "proj-a", "comp-a1", "dev", "puid-a", "cuid-a1", ""}},
		{"everything", identityB},
	}

	for _, tt := range tests {
		// The segment's own annotations say A1 but its resource metadata
		// disagrees or is incomplete. The names cannot be attributed to the
		// identity the span list filters on, so none are emitted.
		client := newTraceClient(t,
			testSegment("segA1", "a1", identityA1.annotations(), tt.metadata.metadata()),
		)
		result, err := client.GetSpanDetail(context.Background(), TracesQueryParams{
			TraceID: "5759e988bd862e3fe1be46a994272793",
			SpanID:  "segA1",
		})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tt.name, err)
		}
		assertResourceAttrs(t, tt.name, result.Span.ResourceAttributes, identityOnlyResource(identityA1),
			append([]string{"service.name"}, resourceNameKeys...)...)
	}
}

func TestResourceInheritance_RequiresCompleteMatchingIdentity(t *testing.T) {
	withA1 := func(project, component, environment string) testIdentity {
		return testIdentity{"ns-a", "proj-x", "comp-x", "env-x", project, component, environment}
	}
	incompleteA1 := testIdentity{"ns-a", "proj-a", "comp-a1", "dev", "puid-a", "", "euid-dev"}

	tests := []struct {
		name    string
		segment testIdentity
		child   testIdentity
		inherit bool
	}{
		{"complete match", identityA1, identityA1, true},
		{"other project uid", identityA1, withA1("puid-x", "cuid-a1", "euid-dev"), false},
		{"other component uid", identityA1, withA1("puid-a", "cuid-x", "euid-dev"), false},
		{"other environment uid", identityA1, withA1("puid-a", "cuid-a1", "euid-x"), false},
		{"child without project uid", identityA1, withA1("", "cuid-a1", "euid-dev"), false},
		{"child without component uid", identityA1, withA1("puid-a", "", "euid-dev"), false},
		{"child without environment uid", identityA1, withA1("puid-a", "cuid-a1", ""), false},
		// The segment itself lacks the component UID in both its annotations
		// and its metadata, so a child of any component in the same project
		// and environment would otherwise look identical.
		{"both without component uid", incompleteA1, testIdentity{"ns-a", "", "", "", "puid-a", "", "euid-dev"}, false},
	}

	for _, tt := range tests {
		trace := testSegment("seg", "seg", tt.segment.annotations(), tt.segment.metadata(),
			testSubsegment("child", tt.child.annotations()))
		wantOwn, absentOwn := ownIdentityResource(tt.child)
		want := map[string]string{}
		absent := absentOwn
		if tt.inherit {
			for k, v := range fullResource(tt.segment) {
				want[k] = v
			}
			want["service.name"] = tt.segment.component + "-svc"
		} else {
			for k, v := range wantOwn {
				want[k] = v
			}
			absent = append(append([]string{"service.name"}, resourceNameKeys...), absentOwn...)
		}

		detail, err := newTraceClient(t, trace).GetSpanDetail(context.Background(), TracesQueryParams{
			TraceID: "5759e988bd862e3fe1be46a994272793",
			SpanID:  "child",
		})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tt.name, err)
		}
		assertResourceAttrs(t, tt.name+" (details)", detail.Span.ResourceAttributes, want, absent...)

		list, err := newTraceClient(t, trace).GetSpans(context.Background(), TracesQueryParams{
			TraceID:           "5759e988bd862e3fe1be46a994272793",
			Scope:             Scope{Namespace: "ns-a"},
			IncludeAttributes: true,
		})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tt.name, err)
		}
		found := false
		for _, s := range list.Spans {
			if s.SpanID == "child" {
				found = true
				assertResourceAttrs(t, tt.name+" (list)", s.ResourceAttributes, want, absent...)
			}
		}
		if !found {
			t.Errorf("%s: expected child in span list, got %v", tt.name, spanIDs(list.Spans))
		}
	}
}

func TestGetSpanDetail_IncompleteSegmentIdentityGetsNoNames(t *testing.T) {
	// A segment without a component UID cannot have its names attributed to
	// a complete identity, so it carries only what its annotations establish.
	incomplete := testIdentity{"ns-a", "proj-a", "comp-a1", "dev", "puid-a", "", "euid-dev"}
	client := newTraceClient(t, testSegment("seg", "seg", incomplete.annotations(), incomplete.metadata()))
	result, err := client.GetSpanDetail(context.Background(), TracesQueryParams{
		TraceID: "5759e988bd862e3fe1be46a994272793",
		SpanID:  "seg",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want, absent := ownIdentityResource(incomplete)
	assertResourceAttrs(t, "seg", result.Span.ResourceAttributes, want,
		append(append([]string{"service.name"}, resourceNameKeys...), absent...)...)
}

func TestResourceInheritance_EmptyUIDIsMissing(t *testing.T) {
	// The segment, its metadata and the child all carry the component UID as
	// an empty string rather than leaving it out. An empty UID identifies no
	// component, so it must neither be emitted nor let the child match the
	// segment and inherit its names.
	withEmptyComponentUID := func() map[string]interface{} {
		annotations := identityA1.annotations()
		annotations["openchoreo.dev_component_uid"] = ""
		return annotations
	}
	metadata := identityA1.metadata()
	metadata["default"].(map[string]interface{})["otel.resource.openchoreo.dev/component-uid"] = ""
	trace := testSegment("seg", "seg", withEmptyComponentUID(), metadata,
		testSubsegment("child", withEmptyComponentUID()))

	want := identityOnlyResource(identityA1)
	delete(want, "openchoreo.dev/component-uid")
	absent := append([]string{"openchoreo.dev/component-uid", "service.name"}, resourceNameKeys...)

	for _, spanID := range []string{"seg", "child"} {
		detail, err := newTraceClient(t, trace).GetSpanDetail(context.Background(), TracesQueryParams{
			TraceID: "5759e988bd862e3fe1be46a994272793",
			SpanID:  spanID,
		})
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", spanID, err)
		}
		assertResourceAttrs(t, spanID+" (details)", detail.Span.ResourceAttributes, want, absent...)
	}

	list, err := newTraceClient(t, trace).GetSpans(context.Background(), TracesQueryParams{
		TraceID:           "5759e988bd862e3fe1be46a994272793",
		Scope:             Scope{Namespace: "ns-a"},
		IncludeAttributes: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := spanIDs(list.Spans); !equalIDs(got, []string{"child", "seg"}) {
		t.Fatalf("expected spans [child seg], got %v", got)
	}
	for _, s := range list.Spans {
		assertResourceAttrs(t, s.SpanID+" (list)", s.ResourceAttributes, want, absent...)
	}
}

func TestResourceInheritance_SegmentIdentityFromAnnotations(t *testing.T) {
	// The segment's annotations say A1, but its resource metadata describes B
	// in full. A nested subsegment that belongs to B must not inherit B's
	// names through the segment: they were exported on a span whose indexed
	// identity is A1, so they cannot be attributed to B.
	trace := testSegment("segA1", "a1", identityA1.annotations(), identityB.metadata(),
		testSubsegment("subB", identityB.annotations()))
	absent := append([]string{"service.name"}, resourceNameKeys...)

	detail, err := newTraceClient(t, trace).GetSpanDetail(context.Background(), TracesQueryParams{
		TraceID: "5759e988bd862e3fe1be46a994272793",
		SpanID:  "subB",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertResourceAttrs(t, "subB (details)", detail.Span.ResourceAttributes, identityOnlyResource(identityB), absent...)

	list, err := newTraceClient(t, trace).GetSpans(context.Background(), TracesQueryParams{
		TraceID:           "5759e988bd862e3fe1be46a994272793",
		Scope:             Scope{Namespace: "ns-b"},
		IncludeAttributes: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := spanIDs(list.Spans); !equalIDs(got, []string{"subB"}) {
		t.Fatalf("expected spans [subB], got %v", got)
	}
	assertResourceAttrs(t, "subB (list)", list.Spans[0].ResourceAttributes, identityOnlyResource(identityB), absent...)
}

func timedSegment(id string, start, end string, annotations map[string]interface{}, subsegments ...map[string]interface{}) map[string]interface{} {
	seg := testSegment(id, id, annotations, nil, subsegments...)
	seg["start_time"] = json.Number(start)
	seg["end_time"] = json.Number(end)
	return seg
}

func timedSubsegment(id string, start, end string, annotations map[string]interface{}) map[string]interface{} {
	sub := testSubsegment(id, annotations)
	sub["start_time"] = json.Number(start)
	sub["end_time"] = json.Number(end)
	return sub
}

func TestGetSpans_TimeRangeAppliedBeforeCountAndLimit(t *testing.T) {
	a1 := identityA1.annotations()
	client := newTraceClient(t,
		timedSegment("startsBefore", "1465510987.999999999", "1465510988.5", a1,
			timedSubsegment("inside", "1465510988.2", "1465510988.8", a1),
			timedSubsegment("endsAfter", "1465510988.5", "1465510989.000000001", a1)),
		timedSegment("startOnBoundary", "1465510988", "1465510988.5", a1),
		timedSegment("endOnBoundary", "1465510988.5", "1465510989", a1),
		timedSegment("outside", "1465510990", "1465510991", a1),
		timedSegment("foreignInside", "1465510988.2", "1465510988.8", identityB.annotations()),
	)
	window := TracesQueryParams{
		TraceID:   "5759e988bd862e3fe1be46a994272793",
		Scope:     Scope{Namespace: "ns-a"},
		StartTime: time.Unix(1465510988, 0),
		EndTime:   time.Unix(1465510989, 0),
	}

	result, err := client.GetSpans(context.Background(), window)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"inside", "startOnBoundary", "endOnBoundary"}
	if got := spanIDs(result.Spans); !equalIDs(got, append([]string(nil), want...)) {
		t.Errorf("expected spans %v, got %v", want, got)
	}
	if result.Total != 3 {
		t.Errorf("expected total 3, got %d", result.Total)
	}

	window.Limit = 2
	limited, err := client.GetSpans(context.Background(), window)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if limited.Total != 3 || len(limited.Spans) != 2 {
		t.Errorf("expected total 3 and 2 spans after limit, got total %d and %v", limited.Total, spanIDs(limited.Spans))
	}
	for _, s := range limited.Spans {
		switch s.SpanID {
		case "inside", "startOnBoundary", "endOnBoundary":
		default:
			t.Errorf("limit applied before the time filter: got %s", s.SpanID)
		}
	}
}

func TestGetSpans_MissingEndTimeExcludedWhenUpperBoundSet(t *testing.T) {
	a1 := identityA1.annotations()
	inProgressSeg := timedSegment("inProgressSegment", "1465510990", "0", a1,
		timedSubsegment("childInside", "1465510988.2", "1465510988.8", a1),
		timedSubsegment("inProgressChild", "1465510988.2", "0", a1))
	delete(inProgressSeg, "end_time")
	inProgressSeg["in_progress"] = true
	inProgressChild := inProgressSeg["subsegments"].([]map[string]interface{})[1]
	delete(inProgressChild, "end_time")
	inProgressChild["in_progress"] = true

	client := newTraceClient(t,
		inProgressSeg,
		timedSegment("inside", "1465510988.2", "1465510988.8", a1),
	)
	params := TracesQueryParams{
		TraceID:   "5759e988bd862e3fe1be46a994272793",
		Scope:     Scope{Namespace: "ns-a"},
		StartTime: time.Unix(1465510988, 0),
		EndTime:   time.Unix(1465510989, 0),
		Limit:     1,
	}

	result, err := client.GetSpans(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The in-progress segment and subsegment have no end, so they cannot be
	// shown to end inside the window; the in-window child of the excluded
	// segment is still judged on its own.
	if result.Total != 2 {
		t.Errorf("expected total 2, got %d", result.Total)
	}
	if len(result.Spans) != 1 || (result.Spans[0].SpanID != "childInside" && result.Spans[0].SpanID != "inside") {
		t.Errorf("expected one in-window span after limit, got %v", spanIDs(result.Spans))
	}

	params.Limit = 0
	all, err := client.GetSpans(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := spanIDs(all.Spans), []string{"childInside", "inside"}; !equalIDs(got, want) {
		t.Errorf("expected spans %v, got %v", want, got)
	}

	// Without an upper bound an in-progress span that starts in range is kept.
	params.EndTime = time.Time{}
	unbounded, err := client.GetSpans(context.Background(), params)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := spanIDs(unbounded.Spans), []string{"inProgressSegment", "childInside", "inProgressChild", "inside"}; !equalIDs(got, want) {
		t.Errorf("without an upper bound expected spans %v, got %v", want, got)
	}
}

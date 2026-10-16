// Copyright 2026 The OpenChoreo Authors
// SPDX-License-Identifier: Apache-2.0

package opensearch

import (
	"reflect"
	"testing"
)

// completeAuditSource carries every field the contract requires.
func completeAuditSource() map[string]interface{} {
	return map[string]interface{}{
		"schema_version": "1.0",
		"event_id":       "0192f1a0-0000-7000-8000-000000000001",
		"event_time":     "2026-09-01T10:00:00Z",
		"actor": map[string]interface{}{
			"type": "user",
			"id":   "user-1",
		},
		"action":   "create_project",
		"category": "management",
		"result":   "success",
	}
}

func TestParseAuditRecord_ReadsACompleteRecord(t *testing.T) {
	record, err := ParseAuditRecord(Hit{ID: "doc-1", Source: completeAuditSource()})
	if err != nil {
		t.Fatalf("ParseAuditRecord() error = %v", err)
	}
	if record.EventID != "0192f1a0-0000-7000-8000-000000000001" {
		t.Errorf("eventID = %q", record.EventID)
	}
	if record.Actor.ID != "user-1" || record.Actor.Type != "user" {
		t.Errorf("actor = %+v", record.Actor)
	}
}

func TestParseAuditRecord_ReadsRequestAndReturned(t *testing.T) {
	source := completeAuditSource()
	source["schema_version"] = "1.1"
	source["request"] = map[string]interface{}{
		"parameters": map[string]interface{}{
			"sinceSeconds": float64(3600),
			"actor":        map[string]interface{}{"id": []interface{}{"alice"}},
		},
	}
	source["returned"] = map[string]interface{}{
		"count": float64(3),
		"items": []interface{}{
			map[string]interface{}{"type": "secret", "namespace": "acme", "name": "db-creds", "uid": "uid-a"},
			map[string]interface{}{"type": "secret", "namespace": "acme", "name": "api-key", "uid": "uid-b"},
		},
	}

	record, err := ParseAuditRecord(Hit{ID: "doc-1", Source: source})
	if err != nil {
		t.Fatalf("ParseAuditRecord() error = %v", err)
	}

	if record.Request == nil {
		t.Fatal("request = nil, want the stored parameters")
	}
	wantParameters := map[string]any{
		"sinceSeconds": float64(3600),
		"actor":        map[string]any{"id": []any{"alice"}},
	}
	if !reflect.DeepEqual(record.Request.Parameters, wantParameters) {
		t.Errorf("request.parameters = %v, want %v", record.Request.Parameters, wantParameters)
	}

	wantReturned := &AuditReturned{
		Count: 3,
		Items: []AuditReturnedItem{
			{Type: "secret", Namespace: "acme", Name: "db-creds", UID: "uid-a"},
			{Type: "secret", Namespace: "acme", Name: "api-key", UID: "uid-b"},
		},
	}
	if !reflect.DeepEqual(record.Returned, wantReturned) {
		t.Errorf("returned = %+v, want %+v", record.Returned, wantReturned)
	}
}

func TestParseAuditRecord_LeavesAbsentGroupsNil(t *testing.T) {
	record, err := ParseAuditRecord(Hit{ID: "doc-1", Source: completeAuditSource()})
	if err != nil {
		t.Fatalf("ParseAuditRecord() error = %v", err)
	}
	if record.Request != nil {
		t.Errorf("request = %+v, want nil", record.Request)
	}
	if record.Returned != nil {
		t.Errorf("returned = %+v, want nil", record.Returned)
	}
}

// A blank in any of these would go on the wire as a real reading.
func TestParseAuditRecord_RejectsAMissingIdentityField(t *testing.T) {
	for _, field := range []string{
		"schema_version", "event_id", "event_time", "actor", "result",
	} {
		t.Run(field, func(t *testing.T) {
			source := completeAuditSource()
			delete(source, field)

			if _, err := ParseAuditRecord(Hit{ID: "doc-1", Source: source}); err == nil {
				t.Errorf("expected an error for a document with no %s, got nil", field)
			}
		})
	}
}

// A record with no resolved action carries both action and category empty.
func TestParseAuditRecord_KeepsARecordWithNoResolvedAction(t *testing.T) {
	source := completeAuditSource()
	source["action"] = ""
	source["category"] = ""
	source["result"] = "failure"
	source["actor"] = map[string]interface{}{"type": "anonymous", "id": "anonymous"}

	record, err := ParseAuditRecord(Hit{ID: "doc-1", Source: source})
	if err != nil {
		t.Fatalf("ParseAuditRecord() error = %v, want the record kept", err)
	}
	if record.Result != "failure" || record.Actor.ID != "anonymous" {
		t.Errorf("record = %+v, want the anonymous record intact", record)
	}
}

package mutation

import (
	"strings"
	"testing"

	"github.com/mariesqu/engram/internal/domain"
)

func TestSanitizeTextFields_RemovesOnlyNULFromCanonicalText(t *testing.T) {
	value := func() *string { v := "before\x00after\x01"; return &v }
	tests := []struct {
		name string
		set  func(*domain.Mutation)
		get  func(domain.Mutation) string
	}{
		{"op", func(m *domain.Mutation) { m.Op = domain.Op("up\x00sert\x01") }, func(m domain.Mutation) string { return string(m.Op) }},
		{"sync_id", func(m *domain.Mutation) { m.SyncID = "before\x00after\x01" }, func(m domain.Mutation) string { return m.SyncID }},
		{"session_id", func(m *domain.Mutation) { m.SessionID = "before\x00after\x01" }, func(m domain.Mutation) string { return m.SessionID }},
		{"entity_type", func(m *domain.Mutation) { m.EntityType = domain.EntityType("mem\x00ory\x01") }, func(m domain.Mutation) string { return string(m.EntityType) }},
		{"type", func(m *domain.Mutation) { m.Type = "before\x00after\x01" }, func(m domain.Mutation) string { return m.Type }},
		{"title", func(m *domain.Mutation) { m.Title = "before\x00after\x01" }, func(m domain.Mutation) string { return m.Title }},
		{"content", func(m *domain.Mutation) { m.Content = "before\x00after\x01" }, func(m domain.Mutation) string { return m.Content }},
		{"project", func(m *domain.Mutation) { m.Project = "before\x00after\x01" }, func(m domain.Mutation) string { return m.Project }},
		{"scope", func(m *domain.Mutation) { m.Scope = "before\x00after\x01" }, func(m domain.Mutation) string { return m.Scope }},
		{"topic_key", func(m *domain.Mutation) { m.TopicKey = value() }, func(m domain.Mutation) string { return *m.TopicKey }},
		{"status", func(m *domain.Mutation) { m.Status = value() }, func(m domain.Mutation) string { return *m.Status }},
		{"parent_sync_id", func(m *domain.Mutation) { m.ParentSyncID = value() }, func(m domain.Mutation) string { return *m.ParentSyncID }},
		{"writer_id", func(m *domain.Mutation) { m.WriterID = "before\x00after\x01" }, func(m domain.Mutation) string { return m.WriterID }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m domain.Mutation
			tt.set(&m)
			got := tt.get(SanitizeTextFields(m))
			if strings.ContainsRune(got, '\x00') {
				t.Fatalf("sanitized value still contains U+0000: %q", got)
			}
			if !strings.ContainsRune(got, '\x01') {
				t.Fatalf("sanitizer removed U+0001: %q", got)
			}
		})
	}
}

func TestValidateTextFields_NamesOffendingField(t *testing.T) {
	m := domain.Mutation{Content: "bad\x00content"}
	err := ValidateTextFields(m)
	if err == nil || !strings.Contains(err.Error(), `"content"`) || !strings.Contains(err.Error(), "U+0000") {
		t.Fatalf("ValidateTextFields error = %v, want actionable content/U+0000 error", err)
	}
}

func TestValidateCanonicalPayloadText_RejectsNULInUnknownField(t *testing.T) {
	err := ValidateCanonicalPayloadText([]byte(`{"updated_at":"2026-01-01T00:00:00Z","unknown":"bad\u0000value"}`))
	if err == nil || !strings.Contains(err.Error(), "U+0000") {
		t.Fatalf("ValidateCanonicalPayloadText error = %v, want U+0000 rejection", err)
	}
}

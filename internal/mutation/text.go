package mutation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/mariesqu/engram/internal/domain"
)

// RemoveNUL removes the Unicode NUL code point while preserving every other
// rune. PostgreSQL text and jsonb cannot represent U+0000, even when JSON
// encodes it as \u0000.
func RemoveNUL(value string) string {
	return strings.ReplaceAll(value, "\x00", "")
}

// SanitizeTextFields removes U+0000 from every textual field represented in a
// mutation's canonical payload. Callers must use the returned mutation before
// materializing it, constructing its payload, or deriving its mutation ID.
func SanitizeTextFields(m domain.Mutation) domain.Mutation {
	m.Op = domain.Op(RemoveNUL(string(m.Op)))
	m.SyncID = RemoveNUL(m.SyncID)
	m.SessionID = RemoveNUL(m.SessionID)
	m.EntityType = domain.EntityType(RemoveNUL(string(m.EntityType)))
	m.Type = RemoveNUL(m.Type)
	m.Title = RemoveNUL(m.Title)
	m.Content = RemoveNUL(m.Content)
	m.Project = RemoveNUL(m.Project)
	m.Scope = RemoveNUL(m.Scope)
	m.TopicKey = sanitizeOptionalText(m.TopicKey)
	m.Status = sanitizeOptionalText(m.Status)
	m.ParentSyncID = sanitizeOptionalText(m.ParentSyncID)
	m.WriterID = RemoveNUL(m.WriterID)
	return m
}

// ValidateTextFields rejects a decoded mutation containing U+0000 and names
// the offending canonical field so clients can correct the record.
func ValidateTextFields(m domain.Mutation) error {
	fields := []struct {
		name  string
		value string
	}{
		{"op", string(m.Op)},
		{"sync_id", m.SyncID},
		{"session_id", m.SessionID},
		{"entity_type", string(m.EntityType)},
		{"type", m.Type},
		{"title", m.Title},
		{"content", m.Content},
		{"project", m.Project},
		{"scope", m.Scope},
		{"writer_id", m.WriterID},
	}
	for _, field := range fields {
		if strings.ContainsRune(field.value, '\x00') {
			return fmt.Errorf("canonical field %q contains unsupported U+0000", field.name)
		}
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"topic_key", m.TopicKey},
		{"status", m.Status},
		{"parent_sync_id", m.ParentSyncID},
	} {
		if field.value != nil && strings.ContainsRune(*field.value, '\x00') {
			return fmt.Errorf("canonical field %q contains unsupported U+0000", field.name)
		}
	}
	return nil
}

// ValidateCanonicalPayloadText decodes only the canonical text fields and
// validates their PostgreSQL compatibility. It deliberately does not enforce
// unrelated semantic requirements such as a populated updated_at timestamp.
func ValidateCanonicalPayloadText(payload []byte) error {
	var fields canonicalFields
	if err := json.Unmarshal(payload, &fields); err != nil {
		return fmt.Errorf("decode canonical payload: %w", err)
	}
	if err := ValidateTextFields(domain.Mutation{
		Op:           domain.Op(fields.Op),
		SyncID:       fields.SyncID,
		SessionID:    fields.SessionID,
		EntityType:   domain.EntityType(fields.EntityType),
		Type:         fields.Type,
		Title:        fields.Title,
		Content:      fields.Content,
		Project:      fields.Project,
		Scope:        fields.Scope,
		TopicKey:     fields.TopicKey,
		Status:       fields.Status,
		ParentSyncID: fields.ParentSyncID,
		WriterID:     fields.WriterID,
	}); err != nil {
		return err
	}

	// Scan the complete token stream too. This catches U+0000 in unknown fields,
	// object keys, or an earlier duplicate field that struct decoding would drop,
	// all of which PostgreSQL jsonb must still parse before normalization.
	decoder := json.NewDecoder(bytes.NewReader(payload))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode canonical payload token: %w", err)
		}
		if value, ok := token.(string); ok && strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("canonical payload contains unsupported U+0000")
		}
	}
}

func sanitizeOptionalText(value *string) *string {
	if value == nil {
		return nil
	}
	sanitized := RemoveNUL(*value)
	return &sanitized
}

package domain

import "testing"

// TestNormalizeTopicKey verifies the three canonical cases.

func TestNormalizeTopicKey_EmptyStringFoldedToNil(t *testing.T) {
	empty := ""
	m := Mutation{TopicKey: &empty}
	got := NormalizeTopicKey(m)
	if got.TopicKey != nil {
		t.Errorf("NormalizeTopicKey(&\"\"): got TopicKey=%q, want nil", *got.TopicKey)
	}
}

func TestNormalizeTopicKey_NilPreserved(t *testing.T) {
	m := Mutation{TopicKey: nil}
	got := NormalizeTopicKey(m)
	if got.TopicKey != nil {
		t.Errorf("NormalizeTopicKey(nil): got TopicKey=%q, want nil", *got.TopicKey)
	}
}

func TestNormalizeTopicKey_NonEmptyPreserved(t *testing.T) {
	key := "sdd/auth/design"
	m := Mutation{TopicKey: &key}
	got := NormalizeTopicKey(m)
	if got.TopicKey == nil {
		t.Fatal("NormalizeTopicKey(&\"sdd/auth/design\"): got nil, want non-nil")
	}
	if *got.TopicKey != key {
		t.Errorf("NormalizeTopicKey(&%q): got %q, want %q", key, *got.TopicKey, key)
	}
}

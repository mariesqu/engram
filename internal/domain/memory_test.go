package domain

import "testing"

func TestValidEntityType_KnownValues(t *testing.T) {
	known := []EntityType{
		EntityMemory, EntityChange, EntitySpec,
		EntityTask, EntityStandard, EntityPlan, EntityPrompt,
	}
	for _, et := range known {
		if !ValidEntityType(et) {
			t.Errorf("ValidEntityType(%q) = false; want true", et)
		}
	}
}

func TestValidEntityType_UnknownRejected(t *testing.T) {
	unknown := []EntityType{"unknown", "Memory", "CHANGE", ""}
	for _, et := range unknown {
		if ValidEntityType(et) {
			t.Errorf("ValidEntityType(%q) = true; want false", et)
		}
	}
}

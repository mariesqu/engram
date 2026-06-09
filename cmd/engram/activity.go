package main

import "sync"

// SessionActivity tracks the most-recently-observed user prompt per session so
// that mem_save can auto-capture it when capture_prompt=true (default). Only
// the prompt-relevant state is stored here; nudge, scoring, and recovery-token
// functionality lives in old_code and is not part of this package.
//
// All exported methods are safe for concurrent use.
type SessionActivity struct {
	mu       sync.Mutex
	sessions map[string]*sessionState
}

type sessionState struct {
	currentPrompt *promptContext
}

type promptContext struct {
	project string
	content string
}

// NewSessionActivity returns an initialised *SessionActivity.
func NewSessionActivity() *SessionActivity {
	return &SessionActivity{
		sessions: make(map[string]*sessionState),
	}
}

func (a *SessionActivity) getOrCreate(sessionID string) *sessionState {
	s, ok := a.sessions[sessionID]
	if !ok {
		s = &sessionState{}
		a.sessions[sessionID] = s
	}
	return s
}

// RecordPrompt stores the most-recently-seen user prompt for the session.
// Subsequent calls overwrite the previous value (last-write wins).
func (a *SessionActivity) RecordPrompt(sessionID, project, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.getOrCreate(sessionID)
	s.currentPrompt = &promptContext{project: project, content: content}
}

// CurrentPrompt returns the latest prompt for the session when it belongs to
// the same project as the caller. Returns ("", false) when no prompt has been
// recorded, the content is empty, or the stored project does not match.
func (a *SessionActivity) CurrentPrompt(sessionID, project string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[sessionID]
	if !ok || s.currentPrompt == nil {
		return "", false
	}
	if s.currentPrompt.project != project || s.currentPrompt.content == "" {
		return "", false
	}
	return s.currentPrompt.content, true
}

// ClearSession removes the session entry, freeing memory. Called by
// handleSessionEnd so that stale prompts do not leak across sessions.
func (a *SessionActivity) ClearSession(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, sessionID)
}

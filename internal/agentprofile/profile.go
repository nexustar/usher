// Package agentprofile loads named defaults for creating sessions.
package agentprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/nexustar/usher/internal/core"
)

// Profile is a set of session creation defaults addressed by a user-chosen
// name. That name is the agent's only identifier — pickers, `--agent <name>`
// in chat and the API all use it. Empty fields are intentionally allowed, so a
// profile can pin a backend and model while leaving cwd to the caller.
type Profile struct {
	Name               string   `json:"name"`
	Cwd                string   `json:"cwd,omitempty"`
	Backend            string   `json:"backend,omitempty"`
	Model              string   `json:"model,omitempty"`
	AppendSystemPrompt string   `json:"append_system_prompt,omitempty"`
	ExtraArgs          []string `json:"extra_args,omitempty"`
	AutoApprove        bool     `json:"auto_approve,omitempty"`
	// Builtin marks the default agent while nothing is stored for it.
	Builtin bool `json:"builtin,omitempty"`
}

// DefaultName is the agent a request that names no agent, backend or model is
// created from. It always exists: until one is stored, it is the instance's
// default backend with that backend's default model.
const DefaultName = "default"

type fileFormat struct {
	Agents []Profile `json:"agents"`
}

// Load reads the configured agents from path. A missing file is not an error:
// it is the ordinary state before the first agent is created.
func Load(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s := New(nil)
			s.path = path
			return s, nil
		}
		return nil, fmt.Errorf("read agents file: %w", err)
	}
	var file fileFormat
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse agents file: %w", err)
	}
	// Same rules as the write path, so a hand-edited file can't hold anything
	// the UI would refuse to save.
	seen := make(map[string]struct{}, len(file.Agents))
	for i := range file.Agents {
		p, err := validate(file.Agents[i])
		if err != nil {
			return nil, fmt.Errorf("agents[%d]: %w", i, err)
		}
		if _, ok := seen[p.Name]; ok {
			return nil, fmt.Errorf("duplicate agent name %q", p.Name)
		}
		seen[p.Name] = struct{}{}
		file.Agents[i] = p
	}
	s := New(file.Agents)
	s.path = path
	return s, nil
}

// Store provides lookup while preserving configuration order for the UI.
// New builds one directly from memory; production goes through Load.
type Store struct {
	mu       sync.RWMutex
	path     string
	profiles []Profile
	byName   map[string]Profile
	// defaultBackend is the built-in default agent's backend.
	defaultBackend string
}

// SetDefaultBackend names the backend of the built-in default agent.
func (s *Store) SetDefaultBackend(backend string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultBackend = backend
}

func (s *Store) builtinLocked() Profile {
	return Profile{Name: DefaultName, Backend: s.defaultBackend, Model: "default", Builtin: true}
}

func New(profiles []Profile) *Store {
	s := &Store{
		profiles: append([]Profile(nil), profiles...),
		byName:   make(map[string]Profile, len(profiles)),
	}
	for _, p := range profiles {
		s.byName[p.Name] = p
	}
	return s
}

func (s *Store) List() []Profile {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, stored := s.byName[DefaultName]; stored {
		return append([]Profile(nil), s.profiles...)
	}
	return append([]Profile{s.builtinLocked()}, s.profiles...)
}

// validate checks one profile. The name is kept as typed — no case folding —
// and is otherwise free, CJK included. Each rejected class breaks a surface the
// name travels through: whitespace (chat clients cut `--agent <name>` at the
// first space, so the tail would land in the instruction), unprintable runes
// (names that look identical but never match), and "/" "." ".." (the name is a
// path segment in /api/agents/{name}, which proxies normalize).
func validate(p Profile) (Profile, error) {
	p.Name = strings.TrimSpace(p.Name)
	p.Builtin = false
	if p.Name == "" {
		return Profile{}, errors.New("name is required")
	}
	for _, r := range p.Name {
		if unicode.IsSpace(r) {
			return Profile{}, errors.New("name may not contain spaces")
		}
		if r == '/' || !unicode.IsPrint(r) {
			return Profile{}, errors.New(`name may not contain "/" or invisible characters`)
		}
	}
	if p.Name == "." || p.Name == ".." {
		return Profile{}, errors.New(`name may not be "." or ".."`)
	}
	var args []string
	for _, a := range p.ExtraArgs {
		if a != "" {
			args = append(args, a)
		}
	}
	p.ExtraArgs = args
	// Extra args are backend CLI flags; without a pinned backend they would go
	// to whichever backend the create request picks and break its spawn.
	if len(p.ExtraArgs) > 0 && p.Backend == "" {
		return Profile{}, errors.New("extra_args requires a backend")
	}
	return p, nil
}

func (s *Store) Create(p Profile) (Profile, error) {
	p, err := validate(p)
	if err != nil {
		return Profile{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createLocked(p)
}

func (s *Store) createLocked(p Profile) (Profile, error) {
	if _, exists := s.byName[p.Name]; exists {
		return Profile{}, fmt.Errorf("agent %q already exists", p.Name)
	}
	s.profiles = append(s.profiles, p)
	s.byName[p.Name] = p
	if err := s.persistLocked(); err != nil {
		s.profiles = s.profiles[:len(s.profiles)-1]
		delete(s.byName, p.Name)
		return Profile{}, err
	}
	return p, nil
}

// Update replaces the agent called name. A different name in p renames it —
// the name is the agent's only identifier, so refusing would leave a bad one
// unfixable. Existing `--agent <name>` references are the caller's to warn
// about; nothing in usher resolves an agent after creation.
func (s *Store) Update(name string, p Profile) (Profile, error) {
	name = strings.TrimSpace(name)
	if p.Name == "" {
		p.Name = name
	}
	p, err := validate(p)
	if err != nil {
		return Profile{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.byName[name]
	if name == DefaultName {
		if p.Name != name {
			return Profile{}, errors.New("the default agent cannot be renamed")
		}
		if !exists {
			return s.createLocked(p)
		}
	}
	if !exists {
		return Profile{}, fmt.Errorf("agent %q not found", name)
	}
	if p.Name != name {
		if _, taken := s.byName[p.Name]; taken {
			return Profile{}, fmt.Errorf("agent %q already exists", p.Name)
		}
	}
	for i := range s.profiles {
		if s.profiles[i].Name == name {
			s.profiles[i] = p
			break
		}
	}
	delete(s.byName, name)
	s.byName[p.Name] = p
	if err := s.persistLocked(); err != nil {
		for i := range s.profiles {
			if s.profiles[i].Name == p.Name {
				s.profiles[i] = old
				break
			}
		}
		delete(s.byName, p.Name)
		s.byName[name] = old
		return Profile{}, err
	}
	return p, nil
}

func (s *Store) Delete(name string) error {
	name = strings.TrimSpace(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.byName[name]
	if !exists && name == DefaultName {
		return errors.New("the default agent cannot be deleted")
	}
	if !exists {
		return fmt.Errorf("agent %q not found", name)
	}
	index := -1
	for i := range s.profiles {
		if s.profiles[i].Name == name {
			index = i
			break
		}
	}
	s.profiles = append(s.profiles[:index], s.profiles[index+1:]...)
	delete(s.byName, name)
	if err := s.persistLocked(); err != nil {
		s.profiles = append(s.profiles, Profile{})
		copy(s.profiles[index+1:], s.profiles[index:])
		s.profiles[index] = old
		s.byName[name] = old
		return err
	}
	return nil
}

func (s *Store) persistLocked() error {
	if s.path == "" {
		return errors.New("agents store is not writable")
	}
	data, err := json.MarshalIndent(fileFormat{Agents: s.profiles}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode agents file: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create agents directory: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write agents file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace agents file: %w", err)
	}
	return nil
}

// Resolve turns one create request into session options: non-empty request
// fields override the named agent's defaults. A request that names no agent,
// backend or model is created from the default agent; one that names a backend
// or model without an agent passes through unchanged. Overriding the backend
// drops the agent's model and flags. The "default" model sentinel is
// normalized last, so it still beats a configured profile model on its way to
// the backend default. InitialMessage is the caller's to fill in.
func (s *Store) Resolve(name, cwd, backend, model string) (core.CreateOptions, error) {
	var p Profile
	name = strings.TrimSpace(name)
	if name == "" && backend == "" && model == "" && s != nil {
		name = DefaultName
	}
	if name != "" {
		if s == nil {
			return core.CreateOptions{}, errors.New("agents are not configured")
		}
		s.mu.RLock()
		defer s.mu.RUnlock()
		var ok bool
		if p, ok = s.byName[name]; !ok && name == DefaultName {
			p, ok = s.builtinLocked(), true
		}
		if !ok {
			names := []string{DefaultName}
			for n := range s.byName {
				if n != DefaultName {
					names = append(names, n)
				}
			}
			slices.Sort(names)
			return core.CreateOptions{}, fmt.Errorf("unknown agent %q; configured agents: %s", name, strings.Join(names, ", "))
		}
	}
	if cwd != "" {
		p.Cwd = cwd
	}
	if backend != "" {
		// A model and CLI flags belong to the backend they were written for.
		if p.Backend != "" && p.Backend != backend {
			p.Model, p.ExtraArgs = "", nil
		}
		p.Backend = backend
	}
	if model != "" {
		p.Model = model
	}
	if p.Model == "default" {
		p.Model = ""
	}
	return core.CreateOptions{
		Backend: p.Backend, Cwd: p.Cwd, Model: p.Model,
		AppendSystemPrompt: p.AppendSystemPrompt, ExtraArgs: p.ExtraArgs,
		AutoApprove: p.AutoApprove,
	}, nil
}

package cliapps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

var appNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Manifest is a catalog entry. Commands are executed only through the injected
// guarded exec tool; the manager never creates a second shell path.
type Manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Install     string `json:"install,omitempty"`
	Update      string `json:"update,omitempty"`
	Uninstall   string `json:"uninstall,omitempty"`
	Test        string `json:"test,omitempty"`
}

type Catalog struct {
	FetchedAt time.Time  `json:"fetched_at"`
	Apps      []Manifest `json:"apps"`
}

type Installed struct {
	Manifest    Manifest  `json:"manifest"`
	Workspace   string    `json:"workspace"`
	InstalledAt time.Time `json:"installed_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type State struct { Installed map[string]Installed `json:"installed"` }

type Manager struct {
	mu        sync.Mutex
	statePath string
	cachePath string
	workspace string
	exec      tools.Tool
	now       func() time.Time
}

func New(stateDir, workspace string, execTool tools.Tool) (*Manager, error) {
	if execTool == nil || execTool.Name() != "exec" { return nil, errors.New("cliapps: guarded exec tool is required") }
	ws, err := filepath.Abs(workspace); if err != nil { return nil, fmt.Errorf("cliapps: workspace: %w", err) }
	if err := os.MkdirAll(stateDir, 0o700); err != nil { return nil, fmt.Errorf("cliapps: state dir: %w", err) }
	return &Manager{statePath: filepath.Join(stateDir,"installed.json"), cachePath: filepath.Join(stateDir,"catalog.json"), workspace: ws, exec: execTool, now: time.Now}, nil
}

func ValidateManifest(m Manifest) error {
	if !appNameRE.MatchString(m.Name) { return fmt.Errorf("invalid app name %q", m.Name) }
	if strings.TrimSpace(m.Version)=="" { return errors.New("version is required") }
	if m.Install=="" && m.Test=="" { return errors.New("manifest must define install or test") }
	return nil
}

func (m *Manager) SaveCatalog(c Catalog) error {
	m.mu.Lock(); defer m.mu.Unlock()
	seen:=map[string]bool{}
	for _, a := range c.Apps { if err:=ValidateManifest(a); err!=nil{return err}; if seen[a.Name]{return fmt.Errorf("duplicate app %q",a.Name)}; seen[a.Name]=true }
	if c.FetchedAt.IsZero(){c.FetchedAt=m.now().UTC()}
	return writeJSONAtomic(m.cachePath,c)
}

func (m *Manager) Catalog(maxAge time.Duration) (Catalog, bool, error) {
	m.mu.Lock(); defer m.mu.Unlock()
	var c Catalog; if err:=readJSON(m.cachePath,&c); err!=nil { if errors.Is(err,os.ErrNotExist){return Catalog{},false,nil}; return Catalog{},false,err }
	fresh:=maxAge<=0 || m.now().Sub(c.FetchedAt)<=maxAge
	return c,fresh,nil
}

func (m *Manager) List() ([]Installed,error) {
	m.mu.Lock(); defer m.mu.Unlock(); s,err:=m.loadState(); if err!=nil{return nil,err}
	out:=make([]Installed,0,len(s.Installed)); for _,v:=range s.Installed{out=append(out,v)}
	sort.Slice(out,func(i,j int)bool{return out[i].Manifest.Name<out[j].Manifest.Name}); return out,nil
}

func (m *Manager) Install(ctx context.Context, a Manifest) error {
	if err:=ValidateManifest(a); err!=nil{return err}; if a.Install==""{return errors.New("install command is not defined")}
	m.mu.Lock(); defer m.mu.Unlock(); s,err:=m.loadState(); if err!=nil{return err}; if _,ok:=s.Installed[a.Name];ok{return fmt.Errorf("app %q already installed",a.Name)}
	if err=m.run(ctx,a.Install);err!=nil{return fmt.Errorf("install %s: %w",a.Name,err)}
	now:=m.now().UTC(); s.Installed[a.Name]=Installed{Manifest:a,Workspace:m.workspace,InstalledAt:now,UpdatedAt:now}; return writeJSONAtomic(m.statePath,s)
}

func (m *Manager) Update(ctx context.Context, a Manifest) error {
	if err:=ValidateManifest(a);err!=nil{return err}; if a.Update==""{return errors.New("update command is not defined")}
	m.mu.Lock(); defer m.mu.Unlock(); s,err:=m.loadState();if err!=nil{return err}; old,ok:=s.Installed[a.Name];if !ok{return fmt.Errorf("app %q is not installed",a.Name)}
	if err=m.run(ctx,a.Update);err!=nil{return fmt.Errorf("update %s: %w",a.Name,err)}; old.Manifest=a;old.UpdatedAt=m.now().UTC();s.Installed[a.Name]=old;return writeJSONAtomic(m.statePath,s)
}

func (m *Manager) Uninstall(ctx context.Context,name string) error {
	if !appNameRE.MatchString(name){return fmt.Errorf("invalid app name %q",name)};m.mu.Lock();defer m.mu.Unlock();s,err:=m.loadState();if err!=nil{return err};old,ok:=s.Installed[name];if !ok{return fmt.Errorf("app %q is not installed",name)}
	if old.Manifest.Uninstall!=""{if err=m.run(ctx,old.Manifest.Uninstall);err!=nil{return fmt.Errorf("uninstall %s: %w",name,err)}};delete(s.Installed,name);return writeJSONAtomic(m.statePath,s)
}

func (m *Manager) Test(ctx context.Context,name string) error {
	m.mu.Lock();defer m.mu.Unlock();s,err:=m.loadState();if err!=nil{return err};old,ok:=s.Installed[name];if !ok{return fmt.Errorf("app %q is not installed",name)};if old.Manifest.Test==""{return errors.New("test command is not defined")};return m.run(ctx,old.Manifest.Test)
}

func (m *Manager) run(ctx context.Context, command string) error {
	raw,_:=json.Marshal(map[string]any{"command":command,"working_dir":m.workspace}); r,err:=m.exec.Execute(ctx,raw);if err!=nil{return err};if r.IsError{return errors.New(r.Content)};return nil
}

func (m *Manager) loadState()(State,error){var s State;err:=readJSON(m.statePath,&s);if errors.Is(err,os.ErrNotExist){return State{Installed:map[string]Installed{}},nil};if err!=nil{return State{},err};if s.Installed==nil{s.Installed=map[string]Installed{}};return s,nil}

func readJSON(path string,v any)error{b,err:=os.ReadFile(path);if err!=nil{return err};if err=json.Unmarshal(b,v);err!=nil{return fmt.Errorf("cliapps: decode %s: %w",filepath.Base(path),err)};return nil}
func writeJSONAtomic(path string,v any)error{b,err:=json.MarshalIndent(v,"","  ");if err!=nil{return err};tmp:=path+".tmp";if err=os.WriteFile(tmp,b,0o600);err!=nil{return err};if err=os.Rename(tmp,path);err!=nil{_ = os.Remove(tmp);return err};return nil}

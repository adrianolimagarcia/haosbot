package cliapps

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

type fakeExec struct{ calls []map[string]any; fail bool }
func (f *fakeExec) Name()string{return "exec"}
func (f *fakeExec) Description()string{return ""}
func (f *fakeExec) Parameters()json.RawMessage{return json.RawMessage(`{}`)}
func (f *fakeExec) Execute(_ context.Context,b json.RawMessage)(tools.Result,error){var v map[string]any;if err:=json.Unmarshal(b,&v);err!=nil{return tools.Result{},err};f.calls=append(f.calls,v);if f.fail{return tools.Result{Content:"blocked",IsError:true},nil};return tools.OK("ok"),nil}

type wrongTool struct{ fakeExec }
func (*wrongTool) Name()string{return "shell"}

func TestNewRequiresGuardedExec(t *testing.T){if _,err:=New(t.TempDir(),t.TempDir(),&wrongTool{});err==nil{t.Fatal("expected guarded exec validation")}}

func TestLifecyclePersistsAndBindsWorkspace(t *testing.T){
	state:=t.TempDir();ws:=t.TempDir();x:=&fakeExec{};m,err:=New(state,ws,x);if err!=nil{t.Fatal(err)}
	clock:=time.Date(2026,9,20,1,2,3,0,time.UTC);m.now=func()time.Time{return clock}
	a:=Manifest{Name:"demo",Version:"1.0.0",Install:"install-demo",Update:"update-demo",Uninstall:"remove-demo",Test:"test-demo"}
	if err=m.Install(context.Background(),a);err!=nil{t.Fatal(err)}
	got,err:=m.List();if err!=nil||len(got)!=1{t.Fatalf("list=%v err=%v",got,err)}
	abs,_:=filepath.Abs(ws);if got[0].Workspace!=abs{t.Fatalf("workspace=%q want %q",got[0].Workspace,abs)}
	if x.calls[0]["working_dir"]!=abs{t.Fatalf("exec not workspace-bound: %#v",x.calls[0])}
	a.Version="2.0.0";if err=m.Update(context.Background(),a);err!=nil{t.Fatal(err)};if err=m.Test(context.Background(),"demo");err!=nil{t.Fatal(err)}
	m2,err:=New(state,ws,x);if err!=nil{t.Fatal(err)};got,err=m2.List();if err!=nil||got[0].Manifest.Version!="2.0.0"{t.Fatalf("persisted=%v err=%v",got,err)}
	if err=m2.Uninstall(context.Background(),"demo");err!=nil{t.Fatal(err)};got,err=m2.List();if err!=nil||len(got)!=0{t.Fatalf("after uninstall=%v err=%v",got,err)}
}

func TestFailedCommandDoesNotMutateState(t *testing.T){m,err:=New(t.TempDir(),t.TempDir(),&fakeExec{fail:true});if err!=nil{t.Fatal(err)};a:=Manifest{Name:"demo",Version:"1",Install:"bad"};if err=m.Install(context.Background(),a);err==nil{t.Fatal("expected command failure")};got,err:=m.List();if err!=nil||len(got)!=0{t.Fatalf("state mutated: %v %v",got,err)}}

func TestCatalogCacheAndValidation(t *testing.T){m,err:=New(t.TempDir(),t.TempDir(),&fakeExec{});if err!=nil{t.Fatal(err)};now:=time.Date(2026,9,20,0,0,0,0,time.UTC);m.now=func()time.Time{return now};c:=Catalog{Apps:[]Manifest{{Name:"demo",Version:"1",Install:"ok"}}};if err=m.SaveCatalog(c);err!=nil{t.Fatal(err)};_,fresh,err:=m.Catalog(time.Hour);if err!=nil||!fresh{t.Fatalf("fresh=%v err=%v",fresh,err)};m.now=func()time.Time{return now.Add(2*time.Hour)};_,fresh,err=m.Catalog(time.Hour);if err!=nil||fresh{t.Fatalf("fresh=%v err=%v",fresh,err)};if err=m.SaveCatalog(Catalog{Apps:[]Manifest{{Name:"../bad",Version:"1",Install:"x"}}});err==nil{t.Fatal("expected invalid manifest")}}

func TestDeterministicErrors(t *testing.T){m,err:=New(t.TempDir(),t.TempDir(),&fakeExec{});if err!=nil{t.Fatal(err)};if err=m.Uninstall(context.Background(),"missing");err==nil{t.Fatal("expected not installed")};if !errors.Is(context.Canceled,context.Canceled){t.Fatal("unreachable")}}

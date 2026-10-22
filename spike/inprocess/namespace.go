package inprocess

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	gms "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/memory"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/analyzer"
	"github.com/dolthub/go-mysql-server/sql/types"
)

type Options struct {
	ReadOnly bool
	Locked   bool
}

type Namespace struct {
	Name     string
	DB       *memory.Database
	Engine   *gms.Engine
	Provider *memory.DbProvider
	other    *Namespace
}

var (
	sharedEngine *gms.Engine
	sharedProv   *memory.DbProvider
	sharedNames  = map[string]bool{}
	sharedMu     sync.Mutex
	engineOpts   Options
)

func engineFor(opts Options) *gms.Engine {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedEngine == nil {
		sharedProv = memory.NewDBProvider()
		analyzer := analyzer.NewDefault(sharedProv)
		sharedEngine = gms.New(analyzer, &gms.Config{
			IsReadOnly:     opts.ReadOnly,
			IsServerLocked: opts.Locked,
		})
		engineOpts = opts
		return sharedEngine
	}
	if engineOpts != opts {
		panic(fmt.Sprintf("inprocess: the engine already exists with confinement %+v and cannot be rebuilt with %+v; go-mysql-server registers global functions and panics on a second engine", engineOpts, opts))
	}
	return sharedEngine
}

type Column struct {
	Name     string
	Type     sql.Type
	Nullable bool
}

type TableSpec struct {
	Name    string
	Columns []Column
	Rows    [][]any
}

func useUnconfinedEngine(t interface{ Cleanup(func()) }) {
	t.Cleanup(resetEngine)
}

func NewNamespace(name string, opts Options) *Namespace {
	engine := engineFor(opts)
	sharedMu.Lock()
	defer sharedMu.Unlock()
	ctx := sql.NewContext(context.Background())
	var db *memory.Database
	if sharedNames[name] {
		got, err := sharedProv.Database(ctx, name)
		if err != nil {
			panic(err)
		}
		db = got.(*memory.Database)
	} else {
		if err := sharedProv.CreateDatabase(ctx, name); err != nil {
			panic(err)
		}
		got, err := sharedProv.Database(ctx, name)
		if err != nil {
			panic(err)
		}
		db = got.(*memory.Database)
		sharedNames[name] = true
	}
	return &Namespace{Name: name, DB: db, Engine: engine, Provider: sharedProv}
}

func (n *Namespace) WithOther(name string, seedFn func(*Namespace) error) (*Namespace, error) {
	other := NewNamespace(name, engineOpts)
	n.other = other
	if seedFn != nil {
		if err := seedFn(other); err != nil {
			return nil, err
		}
	}
	return other, nil
}

func (n *Namespace) Context() *sql.Context {
	session := memory.NewSession(sql.NewBaseSession(), n.Provider)
	ctx := sql.NewContext(context.Background(), sql.WithSession(session))
	ctx.SetCurrentDatabase(n.Name)
	return ctx
}

func (n *Namespace) CreateTable(spec TableSpec) error {
	ctx := n.Context()
	sch := sql.Schema{}
	for _, c := range spec.Columns {
		sch = append(sch, &sql.Column{Name: c.Name, Type: c.Type, Nullable: c.Nullable, Source: spec.Name})
	}
	table := memory.NewTable(n.DB.BaseDatabase, spec.Name, sql.NewPrimaryKeySchema(sch), n.DB.GetForeignKeyCollection())
	n.DB.AddTable(spec.Name, table)
	for _, r := range spec.Rows {
		if err := table.Insert(ctx, sql.NewRow(r...)); err != nil {
			return err
		}
	}
	return nil
}

func (n *Namespace) Query(q string) ([][]any, error) {
	ctx := n.Context()
	sch, iter, _, err := n.Engine.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := drain(ctx, sch, iter)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (n *Namespace) QueryErr(q string) error {
	_, err := n.Query(q)
	return err
}

func drain(ctx *sql.Context, sch sql.Schema, iter sql.RowIter) ([][]any, error) {
	if iter == nil {
		return nil, nil
	}
	defer iter.Close(ctx)
	var out [][]any
	for {
		row, err := iter.Next(ctx)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, []any(row))
	}
}

func Text() sql.Type    { return types.Text }
func Int64() sql.Type   { return types.Int64 }
func Float64() sql.Type { return types.Float64 }
func Bool() sql.Type    { return types.Boolean }
func Blob() sql.Type    { return types.Blob }

func Message(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func useConfinedEngine(t interface{ Cleanup(func()) }) {
	t.Cleanup(resetEngine)
}

func resetEngine() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	sharedEngine = nil
	sharedProv = nil
	sharedNames = map[string]bool{}
	engineOpts = Options{}
}

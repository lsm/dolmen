package api

import (
	"context"
	"encoding/json"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

const constraintReadMessage = "the constraints this request declares or is checked against read the tables they consult, so it requires the read verb on each of them in addition to %s; a foreign key's parent table is named in describe_table"

const parentReadMessage = "this table accepts references, so %s on it requires read on the whole namespace, whether or not any table references it; ask for read on the namespace"

func (s *Server) authorizeConstraints(ctx context.Context, op string, body []byte, t authTarget) error {
	id := auth.IdentityFrom(ctx)
	if id.Principal == auth.AdminPrincipal || s.grants == nil || t.Namespace == "" {
		return nil
	}
	ns, table := normNS(t.Namespace), normTable(t.Table)
	switch op {
	case "create_table":
		var req createTableReq
		_ = json.Unmarshal(body, &req)
		if req.AcceptReferences {
			if err := s.requireRead(ctx, id, auth.Object{Namespace: ns}, parentReadMessage, "create_table"); err != nil {
				return err
			}
		}
		return s.requireConstraintReads(ctx, id, ns, table, req.Constraints, "schema")
	case "migrate":
		var req migrateReq
		_ = json.Unmarshal(body, &req)
		for _, c := range req.Changes {
			switch c.Op {
			case schema.OpSetAcceptReferences:
				if err := s.requireRead(ctx, id, auth.Object{Namespace: ns}, parentReadMessage, "changing accept_references"); err != nil {
					return err
				}
			case schema.OpSetRowAccess:
				sc, err := s.declaredSchema(ctx, ns, table)
				if err != nil {
					return err
				}
				if sc.AcceptReferences {
					if err := s.requireRead(ctx, id, auth.Object{Namespace: ns}, parentReadMessage, "changing row_access"); err != nil {
						return err
					}
				}
			case schema.OpAddConstraint:
				if c.Constraint != nil {
					if err := s.requireConstraintReads(ctx, id, ns, table, []schema.Constraint{*c.Constraint}, "schema"); err != nil {
						return err
					}
				}
			case schema.OpDropConstraint:
				constraints, err := s.declaredNamed(ctx, ns, table, c.Name)
				if err != nil {
					return err
				}
				if err := s.requireConstraintReads(ctx, id, ns, table, constraints, "schema"); err != nil {
					return err
				}
			}
		}
	case "drop_table":
		sc, err := s.declaredSchema(ctx, ns, table)
		if err != nil {
			return err
		}
		if sc.AcceptReferences {
			return s.requireRead(ctx, id, auth.Object{Namespace: ns}, parentReadMessage, "drop_table")
		}
	case "insert", "import_records", "relate", "update", "upsert", "upsert_by_key", "delete":
		return s.authorizeConstrainedWrite(ctx, op, ns, table)
	}
	return nil
}

func (s *Server) authorizeConstrainedWrite(ctx context.Context, op, ns, table string) error {
	id := auth.IdentityFrom(ctx)
	if id.Principal == auth.AdminPrincipal || s.grants == nil {
		return nil
	}
	sc, err := s.declaredSchema(ctx, ns, table)
	if err != nil {
		return err
	}
	if op == "delete" && sc.AcceptReferences {
		if err := s.requireRead(ctx, id, auth.Object{Namespace: ns}, parentReadMessage, "delete"); err != nil {
			return err
		}
	}
	own := sc.RowAccess == schema.RowAccessOwn
	if own && (op == "update" || op == "upsert" || op == "upsert_by_key") {
		wide, err := s.holdsTableWideRead(ctx, ns, table)
		if err != nil {
			return err
		}
		own = !wide
	}
	for _, c := range sc.Constraints {
		target := table
		if c.Kind == schema.ConstraintForeignKey {
			if op == "delete" || c.References == nil {
				continue
			}
			target = c.References.Table
		}
		held, err := s.grants.EffectiveVerbs(ctx, id, auth.Object{Namespace: ns, Table: target})
		if err != nil {
			return err
		}
		if held.Has(auth.VerbRead) || own && held.HasAny(dataVerbs...) {
			continue
		}
		return derr.New(derr.Forbidden, constraintReadMessage, "the write verb")
	}
	return nil
}

func (s *Server) requireConstraintReads(ctx context.Context, id auth.Identity, ns, table string, constraints []schema.Constraint, verb string) error {
	if len(constraints) == 0 {
		return nil
	}
	targets := []string{table}
	for _, c := range constraints {
		if c.Kind == schema.ConstraintForeignKey && c.References != nil {
			targets = append(targets, normTable(c.References.Table))
		}
	}
	for _, target := range targets {
		if err := s.requireRead(ctx, id, auth.Object{Namespace: ns, Table: target}, constraintReadMessage, verb); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) requireRead(ctx context.Context, id auth.Identity, obj auth.Object, message, subject string) error {
	held, err := s.grants.EffectiveVerbs(ctx, id, obj)
	if err != nil {
		return err
	}
	if !held.Has(auth.VerbRead) {
		return derr.New(derr.Forbidden, message, subject)
	}
	return nil
}

func (s *Server) declaredSchema(ctx context.Context, ns, table string) (*schema.TableSchema, error) {
	sc, _, err := s.eng.TableState(ctx, ns, table, nil)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	return sc, nil
}

func (s *Server) declaredNamed(ctx context.Context, ns, table, name string) ([]schema.Constraint, error) {
	sc, err := s.declaredSchema(ctx, ns, table)
	if err != nil {
		return nil, err
	}
	for _, c := range sc.Constraints {
		if c.Name == name {
			return []schema.Constraint{c}, nil
		}
	}
	return nil, nil
}

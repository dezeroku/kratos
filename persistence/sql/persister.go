// Copyright © 2022 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package sql

import (
	"context"
	"embed"
	"fmt"
	"time"

	"github.com/ory/x/contextx"

	"github.com/ory/x/fsx"

	"github.com/gobuffalo/pop/v6"
	"github.com/gobuffalo/pop/v6/columns"
	"github.com/gofrs/uuid"
	"github.com/pkg/errors"

	"github.com/ory/x/networkx"
	"github.com/ory/x/sqlcon"

	"github.com/ory/x/popx"

	"github.com/ory/kratos/driver/config"
	"github.com/ory/kratos/identity"
	"github.com/ory/kratos/persistence"
	"github.com/ory/kratos/schema"
	"github.com/ory/kratos/x"
)

var _ persistence.Persister = new(Persister)

//go:embed migrations/sql/*.sql
var migrations embed.FS

type (
	persisterDependencies interface {
		IdentityTraitsSchemas(ctx context.Context) (schema.Schemas, error)
		identity.ValidationProvider
		x.LoggingProvider
		config.Provider
		contextx.Provider
		x.TracingProvider
	}
	Persister struct {
		nid      uuid.UUID
		c        *pop.Connection
		mb       *popx.MigrationBox
		mbs      popx.MigrationStatuses
		r        persisterDependencies
		p        *networkx.Manager
		isSQLite bool
	}
)

func NewPersister(ctx context.Context, r persisterDependencies, c *pop.Connection) (*Persister, error) {
	m, err := popx.NewMigrationBox(fsx.Merge(migrations, networkx.Migrations), popx.NewMigrator(c, r.Logger(), r.Tracer(ctx), 0))
	if err != nil {
		return nil, err
	}
	m.DumpMigrations = false

	return &Persister{
		c: c, mb: m, r: r, isSQLite: c.Dialect.Name() == "sqlite3",
		p: networkx.NewManager(c, r.Logger(), r.Tracer(ctx)),
	}, nil
}

func (p *Persister) NetworkID(ctx context.Context) uuid.UUID {
	return p.r.Contextualizer().Network(ctx, p.nid)
}

func (p Persister) WithNetworkID(sid uuid.UUID) persistence.Persister {
	p.nid = sid
	return &p
}

func (p *Persister) DetermineNetwork(ctx context.Context) (*networkx.Network, error) {
	return p.p.Determine(ctx)
}

func (p *Persister) Connection(ctx context.Context) *pop.Connection {
	return p.c.WithContext(ctx)
}

func (p *Persister) MigrationStatus(ctx context.Context) (popx.MigrationStatuses, error) {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.MigrationStatus")
	defer span.End()

	if p.mbs != nil {
		return p.mbs, nil
	}

	status, err := p.mb.Status(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if !status.HasPending() {
		p.mbs = status
	}

	return status, nil
}

func (p *Persister) MigrateDown(ctx context.Context, steps int) error {
	return p.mb.Down(ctx, steps)
}

func (p *Persister) MigrateUp(ctx context.Context) error {
	return p.mb.Up(ctx)
}

func (p *Persister) Migrator() *popx.Migrator {
	return p.mb.Migrator
}

func (p *Persister) Close(ctx context.Context) error {
	return errors.WithStack(p.GetConnection(ctx).Close())
}

func (p *Persister) Ping() error {
	type pinger interface {
		Ping() error
	}

	// This can not be contextualized because of some gobuffalo/pop limitations.
	return errors.WithStack(p.c.Store.(pinger).Ping())
}

type quotable interface {
	Quote(key string) string
}

type node interface {
	GetID() uuid.UUID
	GetNID() uuid.UUID
}

func (p *Persister) CleanupDatabase(ctx context.Context, wait time.Duration, older time.Duration, batchSize int) error {
	currentTime := time.Now().Add(-older)
	p.r.Logger().Printf("Cleaning up records older than %s\n", currentTime)

	p.r.Logger().Println("Cleaning up expired sessions")
	if err := p.DeleteExpiredSessions(ctx, currentTime, batchSize); err != nil {
		return err
	}
	time.Sleep(wait)

	p.r.Logger().Println("Cleaning up expired continuity containers")
	if err := p.DeleteExpiredContinuitySessions(ctx, currentTime, batchSize); err != nil {
		return err
	}
	time.Sleep(wait)

	p.r.Logger().Println("Cleaning up expired login flows")
	if err := p.DeleteExpiredLoginFlows(ctx, currentTime, batchSize); err != nil {
		return err
	}
	time.Sleep(wait)

	p.r.Logger().Println("Cleaning up expired recovery flows")
	if err := p.DeleteExpiredRecoveryFlows(ctx, currentTime, batchSize); err != nil {
		return err
	}
	time.Sleep(wait)

	p.r.Logger().Println("Cleaning up expired registation flows")
	if err := p.DeleteExpiredRegistrationFlows(ctx, currentTime, batchSize); err != nil {
		return err
	}
	time.Sleep(wait)

	p.r.Logger().Println("Cleaning up expired settings flows")
	if err := p.DeleteExpiredSettingsFlows(ctx, currentTime, batchSize); err != nil {
		return err
	}
	time.Sleep(wait)

	p.r.Logger().Println("Cleaning up expired verification flows")
	if err := p.DeleteExpiredVerificationFlows(ctx, currentTime, batchSize); err != nil {
		return err
	}
	time.Sleep(wait)

	p.r.Logger().Println("Successfully cleaned up the latest batch of the SQL database! " +
		"This should be re-run periodically, to be sure that all expired data is purged.")
	return nil
}

func (p *Persister) update(ctx context.Context, v node, columnNames ...string) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.update")
	defer span.End()

	c := p.GetConnection(ctx)
	quoter, ok := c.Dialect.(quotable)
	if !ok {
		return errors.Errorf("store is not a quoter: %T", p.c.Store)
	}

	model := pop.NewModel(v, ctx)
	tn := model.TableName()

	cols := columns.Columns{}
	if len(columnNames) > 0 && tn == model.TableName() {
		cols = columns.NewColumnsWithAlias(tn, model.As, model.IDField())
		cols.Add(columnNames...)
	} else {
		cols = columns.ForStructWithAlias(v, tn, model.As, model.IDField())
	}

	// #nosec
	stmt := fmt.Sprintf("SELECT COUNT(id) FROM %s AS %s WHERE %s.id = ? AND %s.nid = ?",
		quoter.Quote(model.TableName()),
		model.Alias(),
		model.Alias(),
		model.Alias(),
	)

	var count int
	if err := c.Store.GetContext(ctx, &count, c.Dialect.TranslateSQL(stmt), v.GetID(), v.GetNID()); err != nil {
		return sqlcon.HandleError(err)
	} else if count == 0 {
		return errors.WithStack(sqlcon.ErrNoRows)
	}

	// #nosec
	stmt = fmt.Sprintf("UPDATE %s AS %s SET %s WHERE %s AND %s.nid = :nid",
		quoter.Quote(model.TableName()),
		model.Alias(),
		cols.Writeable().QuotedUpdateString(quoter),
		model.WhereNamedID(),
		model.Alias(),
	)

	if _, err := c.Store.NamedExecContext(ctx, stmt, v); err != nil {
		return sqlcon.HandleError(err)
	}
	return nil
}

func (p *Persister) delete(ctx context.Context, v interface{}, id uuid.UUID) error {
	ctx, span := p.r.Tracer(ctx).Tracer().Start(ctx, "persistence.sql.delete")
	defer span.End()

	nid := p.NetworkID(ctx)

	tabler, ok := v.(interface {
		TableName(ctx context.Context) string
	})
	if !ok {
		return errors.Errorf("expected model to have TableName signature but got: %T", v)
	}

	/* #nosec G201 TableName is static */
	count, err := p.GetConnection(ctx).RawQuery(fmt.Sprintf("DELETE FROM %s WHERE id = ? AND nid = ?", tabler.TableName(ctx)),
		id,
		nid,
	).ExecWithCount()
	if err != nil {
		return sqlcon.HandleError(err)
	}
	if count == 0 {
		return errors.WithStack(sqlcon.ErrNoRows)
	}
	return nil
}

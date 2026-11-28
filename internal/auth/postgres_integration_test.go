//go:build integration

package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/krishbansalofficial/tollgate/internal/auth"
	"github.com/krishbansalofficial/tollgate/migrations"
)

func TestPGStore(t *testing.T) {
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("tollgate"), tcpostgres.WithUsername("t"), tcpostgres.WithPassword("t"),
		tcpostgres.BasicWaitStrategies())
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	db := stdlib.OpenDBFromPool(pool)
	prov, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prov.Up(ctx); err != nil {
		t.Fatal(err)
	}

	st := auth.NewPGStore(pool)
	raw, err := st.CreateKey(ctx, "demo", "laptop", 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	// A second key for the same project exercises the project upsert path.
	if _, err := st.CreateKey(ctx, "demo", "ci", 0, 0); err != nil {
		t.Fatal(err)
	}

	h := auth.Hash(raw)
	k, err := st.LookupKey(ctx, h[:])
	if err != nil {
		t.Fatal(err)
	}
	if k.ProjectName != "demo" || k.RPMLimit != 60 || k.TPMLimit != 500 || k.Revoked {
		t.Errorf("limits not inherited/overridden correctly: %+v", k)
	}
	if k.BudgetUSD != 0 {
		t.Errorf("new project should have no budget: %+v", k)
	}
	if n, err := st.SetBudget(ctx, "demo", 12.5); err != nil || n != 1 {
		t.Fatalf("set budget: n=%d err=%v", n, err)
	}
	if k, _ := st.LookupKey(ctx, h[:]); k.BudgetUSD != 12.5 {
		t.Errorf("budget not loaded: %+v", k)
	}
	if n, _ := st.SetBudget(ctx, "nope", 1); n != 0 {
		t.Errorf("budget set on a missing project")
	}
	if _, err := st.SetBudget(ctx, "demo", 0); err != nil {
		t.Fatal(err)
	}
	if k, _ := st.LookupKey(ctx, h[:]); k.BudgetUSD != 0 {
		t.Errorf("budget 0 should remove it: %+v", k)
	}

	if n, err := st.RevokeKey(ctx, raw[:11]); err != nil || n != 1 {
		t.Fatalf("revoke: n=%d err=%v", n, err)
	}
	if k, _ := st.LookupKey(ctx, h[:]); !k.Revoked {
		t.Error("key not revoked")
	}

	missing := auth.Hash("tg_missing")
	if _, err := st.LookupKey(ctx, missing[:]); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("missing key: %v", err)
	}
}

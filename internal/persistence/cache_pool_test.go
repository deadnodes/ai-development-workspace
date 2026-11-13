package persistence

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"releasecontrol/internal/domain"
)

// Pause the writer just before it reserves the sole connection, then let the
// reader reserve it. The old mutex-before-pool writer order deadlocks here.
type cachePoolTracer struct {
	acquires      atomic.Int32
	writerWaiting chan struct{}
	releaseWriter chan struct{}
	readerQuery   chan struct{}
}

func (tr *cachePoolTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if tr.acquires.Add(1) == 1 {
		close(tr.writerWaiting)
		select {
		case <-tr.releaseWriter:
		case <-ctx.Done():
		}
	}
	return ctx
}
func (*cachePoolTracer) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}
func (tr *cachePoolTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == "SELECT revision FROM control_revision WHERE id=1" {
		close(tr.readerQuery)
	}
	return ctx
}
func (*cachePoolTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPostgresReadUpdateSingleConnection(t *testing.T) {
	fixture := retrievalPostgres(t)
	config, err := pgxpool.ParseConfig(fixture.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1 // pool_max_conns=1, including all concurrent Read/Update calls.
	tracer := &cachePoolTracer{writerWaiting: make(chan struct{}), releaseWriter: make(chan struct{}), readerQuery: make(chan struct{})}
	config.ConnConfig.Tracer = tracer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := &Store{pool: pool}
	results := make(chan error, 2)
	go func() {
		results <- store.Update(ctx, func(st *domain.State) error {
			st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "pool-order"}, Name: "committed"})
			return nil
		})
	}()
	select {
	case <-tracer.writerWaiting:
	case <-ctx.Done():
		t.Fatal("writer did not reach pool acquisition")
	}
	go func() { _, err := store.Read(ctx); results <- err }()
	select {
	case <-tracer.readerQuery:
	case <-ctx.Done():
		t.Fatal("reader did not reserve the sole connection")
	}
	close(tracer.releaseWriter)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("concurrent Read/Update failed: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent Read/Update deadlocked")
		}
	}
	// Use the fixture's independent pool to verify the write actually committed.
	st, err := fixture.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Products) != 1 || st.Products[0].Name != "committed" {
		t.Fatalf("missing committed update: %+v", st.Products)
	}
}

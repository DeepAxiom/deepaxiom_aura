// Package testdb hands each test its own Postgres schema.
//
// It exists because of a failure that only appears when the suite runs for
// real: `go test ./...` runs packages in parallel, so the queue's tests and the
// API's tests were in the same tables at the same time, truncating each other's
// rows. The symptoms were a foreign key violation on an asset another package
// had just removed, a reclaim that found nothing, and a job count that was
// right for neither test. All three read as flakiness, which is the worst way
// for a suite to be wrong: the failure is real, the test is blamed, and the
// next real failure is ignored.
//
// A schema per test is the fix rather than `-p 1`, because serialising the
// suite would only hide it — two people running tests against one development
// database would still collide.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"aura/media/internal/db"
)

// Pool returns a pool whose search_path is a schema created for this test, with
// the media schema already applied inside it. The schema is dropped when the
// test ends.
//
// Skips when AURA_MEDIA_TEST_DSN is unset, so `go test ./...` still runs on a
// machine with no database.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("AURA_MEDIA_TEST_DSN")
	if dsn == "" {
		t.Skip("AURA_MEDIA_TEST_DSN is not set; skipping the tests that need Postgres")
	}
	ctx := context.Background()

	schema := "media_test_" + randomSuffix(t)
	admin, err := db.Open(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("connecting to %s: %v", redact(dsn), err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("creating schema %s: %v", schema, err)
	}

	scoped, err := withSearchPath(dsn, schema)
	if err != nil {
		t.Fatalf("scoping the DSN: %v", err)
	}
	pool, err := db.Open(ctx, scoped, 16)
	if err != nil {
		t.Fatalf("connecting to schema %s: %v", schema, err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("applying the schema: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		// A separate connection: the pool is closed, and dropping a schema from
		// inside a connection whose search_path points at it is asking for
		// trouble.
		cleanup, err := db.Open(context.Background(), dsn, 1)
		if err != nil {
			t.Logf("could not connect to drop schema %s: %v", schema, err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Logf("could not drop schema %s: %v", schema, err)
		}
	})
	return pool
}

// withSearchPath puts the schema in the DSN as a runtime parameter, which is
// how every connection the pool opens lands in it — including the ones it
// opens later, which is why this is not a `SET search_path` on one connection.
func withSearchPath(dsn, schema string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		// A key/value DSN rather than a URL.
		return dsn + " search_path=" + schema, nil
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(buf[:])
}

// redact keeps a password out of a failure message: a test that cannot reach
// its database prints its DSN, and that DSN is a credential.
func redact(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.User == nil {
		return strings.SplitN(dsn, "password=", 2)[0]
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		parsed.User = url.UserPassword(parsed.User.Username(), "***")
	}
	return fmt.Sprint(parsed)
}

package visualstore

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CommittedCounts reads raw committed counts, without interpreting acknowledgements.
// A bounded read-only transaction includes WAL commits. Any read error is doubt;
// callers must not interpret it as an empty store.
func CommittedCounts(dir string) (int64, error) {
	r, err := OpenReader(dir)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int64
	err = r.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT views FROM progress WHERE id=1").Scan(&n)
	})
	if err == nil && n < 0 {
		err = errors.New("negative committed count")
	}
	return n, err
}

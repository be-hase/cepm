package nmhost

import (
	"context"
	"time"

	"github.com/gofrs/flock"
)

// acquireLeadership blocks until this process wins the host lock or ctx ends.
// Chrome can briefly run two host processes (service-worker restart), so new
// processes poll until the old one exits and its lock is released.
func acquireLeadership(ctx context.Context, lockPath string) (release func(), err error) {
	fl := flock.New(lockPath)
	ok, err := fl.TryLockContext(ctx, time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ctx.Err()
	}
	return func() { _ = fl.Unlock() }, nil
}

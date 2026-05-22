package login

import (
	"context"
	"crypto/rand"
	"math"
	"math/big"
	"strconv"

	"github.com/sysop/ultrabridge/internal/spcserver/auth"
)

// ResolveUserID returns the persisted SPC userId, generating and persisting a
// stable one on first call. The key (auth.UserIDSettingKey) is shared with the
// middleware's userId harvest, so a device that presents its real-SPC token
// during cutover adopts that id and ResolveUserID returns it thereafter.
func ResolveUserID(ctx context.Context, store auth.SettingStore) (string, error) {
	existing, err := store.Get(ctx, auth.UserIDSettingKey)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}
	id := generateUserID()
	if err := store.Set(ctx, auth.UserIDSettingKey, id); err != nil {
		return "", err
	}
	return id, nil
}

// generateUserID returns a random 19-digit numeric id that fits a Java Long,
// matching the shape of real SPC userIds (e.g. 1184673925533868032).
func generateUserID() string {
	const min = int64(1_000_000_000_000_000_000) // 1e18 — smallest 19-digit value
	span := big.NewInt(math.MaxInt64 - min)
	n, err := rand.Int(rand.Reader, span)
	if err != nil {
		return strconv.FormatInt(min, 10)
	}
	return strconv.FormatInt(min+n.Int64(), 10)
}
